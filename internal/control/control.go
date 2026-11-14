// Package control holds the service's authoritative playback state — the single
// source of truth that the MQTT command handlers write and the watchdog reads —
// and the one goroutine (Run) that applies it to Home Assistant.
//
// Design: desired state + a single reconciler. Command handlers only record the
// newest intent {on, preset, volume} under the mutex and return immediately; they
// never call HA. Run is the only goroutine that issues HA operations, one at a
// time, always for the latest intent. Why this shape:
//   - Ordering: ops are serialized, and each new power intent (ON/OFF, preset
//     change while ON) cancels the context of the op in flight. That abandons
//     e.g. PlayMedia's unavailable backoff, while a service-call POST already
//     sent is allowed to finish (see ha.Client.callService), so a play from an
//     older intent can never land after a newer OFF.
//   - Non-blocking callers: paho's ordered router calls handlers synchronously
//     and must not block; recording intent is a few field writes under a lock.
//   - Retries: an op stays pending until it succeeds for the intent it was issued
//     for, so a failed media_stop / volume_set / play_media is retried with capped
//     backoff for as long as it is still the latest intent.
//
// Pending work is tracked with want/done counters: every intent that needs a
// power op (play or stop) bumps powerWant, every volume change bumps volWant, and
// the reconciler records as done only the counter value it actually applied, so
// an intent that arrives mid-op stays pending.
//
// State satisfies ha.Controller (IsOn, SuppressedUntil, Replay) so the watchdog
// can be wired straight to it; Replay is a request to the reconciler, not an HA
// call.
package control

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/kevin/ha-white-noise-sonos/internal/ha"
	"github.com/kevin/ha-white-noise-sonos/internal/noise"
)

// suppressWindow is how long the watchdog is told to stand down after a play is
// issued, so a Sonos still buffering (reporting idle) isn't immediately re-played.
const suppressWindow = 30 * time.Second

// Default retry backoff for a failed HA op: doubles from retryBase up to retryMax.
const (
	defaultRetryBase = time.Second
	defaultRetryMax  = time.Minute
)

// Player is the subset of *ha.Client the control state needs to drive playback.
// Abstracted so tests can inject a fake without a live Home Assistant.
type Player interface {
	PlayMedia(ctx context.Context, preset string, volume int) error
	VolumeSet(ctx context.Context, level int) error
	MediaStop(ctx context.Context) error
	GetState(ctx context.Context) (string, error)
}

// State is the authoritative control state. Safe for concurrent use. Construct
// with New and start the reconciler with Run.
type State struct {
	player    Player
	now       func() time.Time
	retryBase time.Duration
	retryMax  time.Duration
	// wake (cap 1) is a coalesced "something changed" signal to the reconciler.
	wake chan struct{}

	mu         sync.Mutex
	on         bool
	preset     string
	volume     int
	suppressed time.Time

	powerWant, powerDone uint64 // power op (play if on, stop if off) pending while different
	ensure               bool   // pending power op is a reassert: play only if not already playing
	volWant, volDone     uint64 // volume_set pending while different
	cancelOp             context.CancelFunc
	// started is true from a successful play_media until a successful
	// media_stop: the speaker may be playing our stream even if the switch is
	// already OFF (stop still pending).
	started bool
}

// New returns a State seeded with the default preset and volume. It does not
// touch Home Assistant until an intent is recorded and Run is running.
func New(player Player, preset string, volume int) *State {
	return &State{
		player:    player,
		now:       time.Now,
		retryBase: defaultRetryBase,
		retryMax:  defaultRetryMax,
		wake:      make(chan struct{}, 1),
		preset:    preset,
		volume:    clampVolume(volume),
	}
}

// Snapshot is an immutable view of the control state for publishing.
type Snapshot struct {
	On     bool
	Preset string
	Volume int
}

// Snapshot returns the current state under lock.
func (s *State) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{On: s.on, Preset: s.preset, Volume: s.volume}
}

// IsOn reports whether the switch is ON. Implements ha.Controller.
func (s *State) IsOn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on
}

// NeedsFinalStop reports whether shutdown should media_stop the target: the
// switch is ON, or this service started playback that no media_stop has ended
// yet (e.g. an OFF whose stop hasn't landed). It is false when the service never
// played, so unrelated music on the speaker is left alone.
func (s *State) NeedsFinalStop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on || s.started
}

// SuppressedUntil returns the instant before which the watchdog must not
// re-play. Zero means not suppressed. Implements ha.Controller.
func (s *State) SuppressedUntil() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suppressed
}

// SetOn records the user's ON/OFF intent. ON → play (volume_set + play_media);
// OFF → media_stop. Either cancels any HA op in flight for an older intent.
// ON while already ON only makes sure the target is playing — it does not
// restart healthy playback.
func (s *State) SetOn(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on && s.on {
		s.ensurePlayingLocked()
		return
	}
	s.on = on
	s.newPowerIntentLocked()
}

// SetPreset changes the preset. If currently ON and the preset changed, it
// re-plays with the new preset URL. Unknown presets are rejected (validated via
// noise.ParsePreset) and leave state unchanged.
func (s *State) SetPreset(preset string) error {
	if _, err := noise.ParsePreset(preset); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := preset != s.preset
	s.preset = preset
	if s.on && changed {
		s.newPowerIntentLocked()
	}
	return nil
}

// SetVolume changes the volume (0–100, clamped) and schedules volume_set on the
// Sonos. The change applies whether or not playback is ON so HA stays in sync.
// It does not cancel an op in flight: a play in progress finishes and the new
// level follows as a volume_set, instead of restarting the play (and its
// fade-in) on every slider step.
func (s *State) SetVolume(level int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = clampVolume(level)
	s.volWant++
	s.poke()
}

// Replay asks the reconciler to re-issue playback for the current preset/volume
// if the switch is still ON. The watchdog calls this on an unexpected
// idle/recovery. Implements ha.Controller. It never blocks and is a no-op while
// OFF. If an unconditional play is already pending it is not restarted; any
// retry wait is just cut short (the watchdog has seen the target change state).
// A pending "play only if not playing" reassert is superseded by an
// unconditional play, since the watchdog has just seen the target not playing:
// it gets a new seq and its in-flight op is cancelled, because that op
// snapshotted ensure and could otherwise trust a stale "playing" and mark the
// replay done.
func (s *State) Replay() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.on {
		return
	}
	if s.powerWant != s.powerDone {
		if s.ensure {
			s.newPowerIntentLocked()
		} else {
			s.poke()
		}
		return
	}
	s.powerWant++
	s.ensure = false
	s.poke()
}

// Reassert re-applies the authoritative state after an MQTT (re)connect. If ON,
// it makes sure the target is playing (no restart if it already is). If OFF it
// does nothing: the speaker may be playing something unrelated, and a broker
// blip or container restart must not stop it.
func (s *State) Reassert() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on {
		s.ensurePlayingLocked()
	}
}

// ensurePlayingLocked schedules a "play only if not already playing" op, unless
// a power op is already pending (which then just gets nudged).
func (s *State) ensurePlayingLocked() {
	if s.powerWant == s.powerDone {
		s.powerWant++
		s.ensure = true
	}
	s.poke()
}

// newPowerIntentLocked schedules an unconditional power op for the current
// on/preset/volume and abandons whatever op is in flight.
func (s *State) newPowerIntentLocked() {
	s.powerWant++
	s.ensure = false
	s.cancelInFlightLocked()
	s.poke()
}

func (s *State) cancelInFlightLocked() {
	if s.cancelOp != nil {
		s.cancelOp()
	}
}

// poke wakes the reconciler without blocking (the signal is coalesced).
func (s *State) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

type opKind int

const (
	opPlay opKind = iota
	opStop
	opVolume
)

func (k opKind) String() string {
	switch k {
	case opPlay:
		return "play"
	case opStop:
		return "stop"
	default:
		return "volume"
	}
}

// op is one HA operation, snapshotted from the desired state under lock.
type op struct {
	kind   opKind
	seq    uint64 // powerWant (play/stop) or volWant (volume) this op satisfies
	volSeq uint64 // play only: volWant at snapshot (a play also applies volume)
	ensure bool   // play only: skip if the target is already playing
	preset string
	volume int
}

// Run is the reconciler: the only goroutine that calls Home Assistant for the
// control state. It applies pending intents one op at a time until ctx is
// cancelled (which also cancels any op in flight). Failed ops are retried with
// capped exponential backoff while they are still the latest intent; a new
// intent interrupts the wait.
func (s *State) Run(ctx context.Context) {
	failures := 0
	for {
		o, opCtx, ok := s.next(ctx)
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}

		played, err := s.apply(opCtx, o)
		superseded := opCtx.Err() != nil // read before finish releases opCtx
		s.finish(o, played, err)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			failures = 0
		case superseded:
			// Cancelled by a newer intent: apply that one straight away.
			failures = 0
		default:
			failures++
			d := retryDelay(failures, s.retryBase, s.retryMax)
			slog.Warn("control: HA operation failed, will retry",
				"op", o.kind.String(), "attempt", failures, "retry_in", d, "err", err)
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-s.wake:
				failures = 0 // new intent or watchdog nudge: try now, backoff afresh
			case <-t.C:
			}
			t.Stop()
		}
	}
}

// next snapshots the next pending op (power before volume) and registers a
// cancellable context for it. ok is false when nothing is pending.
func (s *State) next(ctx context.Context) (o op, opCtx context.Context, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// This snapshot covers every intent recorded so far, so drop any queued wake
	// token; only pokes after this point should interrupt a retry wait.
	select {
	case <-s.wake:
	default:
	}
	switch {
	case s.powerWant != s.powerDone:
		o = op{kind: opStop, seq: s.powerWant}
		if s.on {
			o = op{kind: opPlay, seq: s.powerWant, volSeq: s.volWant, ensure: s.ensure,
				preset: s.preset, volume: s.volume}
		}
	case s.volWant != s.volDone:
		o = op{kind: opVolume, seq: s.volWant, volume: s.volume}
	default:
		return op{}, nil, false
	}
	opCtx, cancel := context.WithCancel(ctx)
	s.cancelOp = cancel
	return o, opCtx, true
}

// apply performs one op against HA. played reports whether play_media was
// actually issued (an ensure-play finds the target already playing).
func (s *State) apply(ctx context.Context, o op) (played bool, err error) {
	switch o.kind {
	case opStop:
		slog.Info("control: stopping playback")
		return false, s.player.MediaStop(ctx)
	case opVolume:
		slog.Debug("control: setting volume", "volume", o.volume)
		return false, s.player.VolumeSet(ctx, o.volume)
	}
	if o.ensure {
		state, err := s.player.GetState(ctx)
		if err != nil {
			return false, err
		}
		if state == ha.StatePlaying {
			slog.Info("control: target already playing, not restarting")
			return false, nil
		}
	}
	slog.Info("control: playing", "preset", o.preset, "volume", o.volume)
	return true, s.player.PlayMedia(ctx, o.preset, o.volume)
}

// finish records a completed op. On success the counters it satisfied are
// marked done (a newer intent keeps them pending) and, after a play, the
// watchdog suppress window starts. A play whose POST got no answer
// (ha.ErrOutcomeUnknown) may have started the speaker, so it counts as started
// for the final stop on shutdown, while staying pending for a retry.
func (s *State) finish(o op, played bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelOp() // release the op context
	s.cancelOp = nil
	if err != nil {
		if o.kind == opPlay && errors.Is(err, ha.ErrOutcomeUnknown) {
			s.started = true
		}
		return
	}
	switch o.kind {
	case opPlay:
		s.powerDone = o.seq
		if played {
			s.volDone = o.volSeq // PlayMedia set this volume first
			s.suppressed = s.now().Add(suppressWindow)
			s.started = true
		}
	case opStop:
		s.powerDone = o.seq
		s.started = false
	case opVolume:
		s.volDone = o.seq
	}
}

// retryDelay is the wait before retry number failures (1-based): base doubling
// each time, capped at maxDelay.
func retryDelay(failures int, base, maxDelay time.Duration) time.Duration {
	d := base
	for i := 1; i < failures && d < maxDelay; i++ {
		d *= 2
	}
	if d > maxDelay {
		d = maxDelay
	}
	return d
}

func clampVolume(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
