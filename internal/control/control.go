// Package control holds the service's authoritative playback state — the single
// source of truth that both the MQTT command handlers and the Phase-3 watchdog
// read and write. State is {on, preset, volume}, guarded by a mutex.
//
// The control state, not stale retained MQTT values, is what gets re-asserted on
// MQTT reconnect (see internal/mqtt reconcile). All HA REST side effects funnel
// through here so play/stop/preset/volume changes apply consistently and the
// watchdog never fights a deliberate stop (we set a suppress window on Stop).
//
// State satisfies ha.Controller (IsOn, SuppressedUntil, Replay) so the watchdog
// can be wired straight to it.
package control

import (
	"context"
	"sync"
	"time"

	"github.com/kevin/ha-white-noise-sonos/internal/noise"
)

// suppressWindow is how long the watchdog is told to stand down after a
// deliberate stop, so it doesn't immediately re-play an intended idle.
const suppressWindow = 15 * time.Second

// Player is the subset of *ha.Client the control state needs to drive playback.
// Abstracted so tests can inject a fake without a live Home Assistant.
type Player interface {
	PlayMedia(ctx context.Context, preset string, volume int) error
	VolumeSet(ctx context.Context, level int) error
	MediaStop(ctx context.Context) error
}

// State is the authoritative control state. Safe for concurrent use. Construct
// with New.
type State struct {
	player Player
	now    func() time.Time

	mu         sync.Mutex
	on         bool
	preset     string
	volume     int
	suppressed time.Time
}

// New returns a State seeded with the default preset and volume. It does not
// touch Home Assistant until an action method is called.
func New(player Player, preset string, volume int) *State {
	return &State{
		player: player,
		now:    time.Now,
		preset: preset,
		volume: clampVolume(volume),
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

// SuppressedUntil returns the instant before which the watchdog must not
// re-play. Zero means not suppressed. Implements ha.Controller.
func (s *State) SuppressedUntil() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suppressed
}

// Replay re-issues playback for the current preset/volume. The watchdog calls
// this on an unexpected idle/recovery. Implements ha.Controller. It is a no-op
// (no error) when the switch is OFF.
func (s *State) Replay(ctx context.Context) error {
	s.mu.Lock()
	if !s.on {
		s.mu.Unlock()
		return nil
	}
	preset, volume := s.preset, s.volume
	s.mu.Unlock()
	return s.player.PlayMedia(ctx, preset, volume)
}

// SetOn turns playback on or off. ON → play_media(current preset) + volume_set.
// OFF → media_stop, and a suppress window so the watchdog doesn't re-play the
// intended idle. Returns the HA error, if any; state is updated regardless so the
// published state reflects the user's intent.
func (s *State) SetOn(ctx context.Context, on bool) error {
	s.mu.Lock()
	s.on = on
	if !on {
		s.suppressed = s.now().Add(suppressWindow)
	} else {
		s.suppressed = time.Time{}
	}
	preset, volume := s.preset, s.volume
	s.mu.Unlock()

	if on {
		return s.player.PlayMedia(ctx, preset, volume)
	}
	return s.player.MediaStop(ctx)
}

// SetPreset changes the preset. If currently ON it re-plays with the new preset
// URL. Unknown presets are rejected (validated via noise.ParsePreset) and leave
// state unchanged.
func (s *State) SetPreset(ctx context.Context, preset string) error {
	if _, err := noise.ParsePreset(preset); err != nil {
		return err
	}
	s.mu.Lock()
	s.preset = preset
	on, volume := s.on, s.volume
	s.mu.Unlock()

	if on {
		return s.player.PlayMedia(ctx, preset, volume)
	}
	return nil
}

// SetVolume changes the volume (0–100, clamped) and issues volume_set on the
// Sonos. The change applies whether or not playback is ON so HA stays in sync.
func (s *State) SetVolume(ctx context.Context, level int) error {
	level = clampVolume(level)
	s.mu.Lock()
	s.volume = level
	s.mu.Unlock()
	return s.player.VolumeSet(ctx, level)
}

// Reassert re-applies the authoritative state to Home Assistant. Used on MQTT
// reconnect: if ON, re-play (preset+volume); if OFF, ensure stopped. Returns the
// first error encountered.
func (s *State) Reassert(ctx context.Context) error {
	s.mu.Lock()
	on, preset, volume := s.on, s.preset, s.volume
	s.mu.Unlock()
	if on {
		return s.player.PlayMedia(ctx, preset, volume)
	}
	return s.player.MediaStop(ctx)
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
