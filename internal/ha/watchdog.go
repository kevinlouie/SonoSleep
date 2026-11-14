package ha

import (
	"context"
	"log/slog"
	"time"
)

// defaultWatchdogInterval is how often the watchdog polls the Sonos state.
const defaultWatchdogInterval = 10 * time.Second

// idlePollsBeforeReplay is how many consecutive idle/paused polls (while ON and
// not suppressed) the watchdog needs before it requests a re-play. One poll is
// not enough: right after play_media the Sonos can still report idle while it
// buffers, and re-playing then would restart the stream and its fade-in.
const idlePollsBeforeReplay = 2

// Controller is the minimal view of the service's control state the watchdog
// needs. Phase 4 (MQTT) supplies the real implementation; this keeps the
// watchdog from hard-depending on MQTT. All methods must be safe to call from the
// watchdog goroutine.
type Controller interface {
	// IsOn reports whether the white-noise switch is currently ON. The watchdog
	// only re-plays while this is true.
	IsOn() bool
	// SuppressedUntil returns the instant before which the watchdog must NOT
	// re-play, even if the Sonos looks idle. Set every time a play is issued so
	// the watchdog doesn't mistake the Sonos buffering for a dropped stream. A
	// zero time means "not suppressed".
	SuppressedUntil() time.Time
	// Replay requests that playback be re-issued (volume_set + play_media) if the
	// switch is still ON. It must not block: the HA calls happen on the
	// controller's own goroutine. The watchdog calls this when it detects an
	// unexpected idle/paused or a recovery from unavailable while ON.
	Replay()
}

// Watchdog polls the target media_player and requests a re-play when the Sonos
// unexpectedly stops (idle/paused on consecutive polls) or recovers from
// unavailable while the service switch is ON. Construct with NewWatchdog.
type Watchdog struct {
	client   stateGetter
	ctrl     Controller
	interval time.Duration
	now      func() time.Time

	// prevState is the last observed state, used to detect the unavailable→
	// available recovery edge.
	prevState string
	// idleStreak counts consecutive idle/paused polls while ON and not suppressed.
	idleStreak int
}

// stateGetter is the subset of *Client the watchdog uses. Lets tests inject a
// fake without standing up a full HTTP server when they only exercise the
// decision logic.
type stateGetter interface {
	GetState(ctx context.Context) (string, error)
}

// NewWatchdog returns a Watchdog that polls client for ctrl's target every
// interval. If interval <= 0 it defaults to 10s.
func NewWatchdog(client stateGetter, ctrl Controller, interval time.Duration) *Watchdog {
	if interval <= 0 {
		interval = defaultWatchdogInterval
	}
	return &Watchdog{
		client:   client,
		ctrl:     ctrl,
		interval: interval,
		now:      time.Now,
	}
}

// Run polls until ctx is cancelled. It is intended to run in its own goroutine.
func (w *Watchdog) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// tick performs one poll-and-maybe-replay cycle. Split out from Run so tests can
// drive the decision logic deterministically without a real ticker.
func (w *Watchdog) tick(ctx context.Context) {
	state, err := w.client.GetState(ctx)
	if err != nil {
		slog.Warn("watchdog: get_state failed", "err", err)
		return
	}
	prev := w.prevState
	w.prevState = state

	if !w.ctrl.IsOn() {
		w.idleStreak = 0
		return // switch is OFF: intended stop, never re-play.
	}
	if prev == StateUnavailable && state != StateUnavailable && state != StatePlaying {
		// Recovery edge: was unavailable, now something else (idle/buffering/on)
		// → nudge playback back straight away. Checked before the suppress
		// window, which is about buffering after a play and must not swallow a
		// one-off edge.
		w.idleStreak = 0
		slog.Info("watchdog: target recovered from unavailable while ON, requesting play", "state", state)
		w.ctrl.Replay()
		return
	}
	if until := w.ctrl.SuppressedUntil(); !until.IsZero() && w.now().Before(until) {
		// A play was just issued (Sonos may still be buffering): don't fight it.
		w.idleStreak = 0
		return
	}

	switch state {
	case StatePlaying, StateUnavailable:
		// Healthy, or still off (the controller's retry handles bring-up).
		w.idleStreak = 0
	case StateIdle, StatePaused:
		w.idleStreak++
		if w.idleStreak < idlePollsBeforeReplay {
			slog.Debug("watchdog: target stopped while ON, confirming on next poll", "state", state)
			return
		}
		w.idleStreak = 0
		slog.Warn("watchdog: target stopped while ON, requesting play", "state", state)
		w.ctrl.Replay()
	default:
		w.idleStreak = 0
	}
}
