package ha

import (
	"context"
	"log"
	"time"
)

// defaultWatchdogInterval is how often the watchdog polls the Sonos state.
const defaultWatchdogInterval = 10 * time.Second

// Controller is the minimal view of the service's control state the watchdog
// needs. Phase 4 (MQTT) supplies the real implementation; this keeps the
// watchdog from hard-depending on MQTT. All methods must be safe to call from the
// watchdog goroutine.
type Controller interface {
	// IsOn reports whether the white-noise switch is currently ON. The watchdog
	// only re-plays while this is true.
	IsOn() bool
	// SuppressedUntil returns the instant before which the watchdog must NOT
	// re-play, even if the Sonos looks idle. Set after a deliberate stop or a
	// known interruption (TTS/alarm announcement) so the watchdog doesn't fight
	// it. A zero time means "not suppressed".
	SuppressedUntil() time.Time
	// Replay re-issues playback (play_media + volume_set). The watchdog calls
	// this when it detects an unexpected idle/paused or a recovery from
	// unavailable while ON.
	Replay(ctx context.Context) error
}

// Watchdog polls the target media_player and re-issues playback when the Sonos
// unexpectedly stops (idle/paused) or recovers from unavailable while the service
// switch is ON. Construct with NewWatchdog.
type Watchdog struct {
	client   stateGetter
	ctrl     Controller
	interval time.Duration
	now      func() time.Time

	// prevState is the last observed state, used to detect the unavailable→
	// available recovery edge.
	prevState string
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
		log.Printf("watchdog: get_state: %v", err)
		return
	}
	prev := w.prevState
	w.prevState = state

	if !w.ctrl.IsOn() {
		return // switch is OFF: intended stop, never re-play.
	}
	if until := w.ctrl.SuppressedUntil(); !until.IsZero() && w.now().Before(until) {
		// Deliberate stop / announcement window: don't fight it.
		return
	}

	switch state {
	case StateIdle, StatePaused:
		log.Printf("watchdog: target %s while ON — re-issuing play", state)
		if err := w.ctrl.Replay(ctx); err != nil {
			log.Printf("watchdog: replay failed: %v", err)
		}
	case StatePlaying:
		// Healthy, nothing to do.
	case StateUnavailable:
		// Still off; nothing to do (PlayMedia's own backoff handles bring-up).
	default:
		// Recovery edge: was unavailable, now something else (buffering/on) →
		// nudge playback back.
		if prev == StateUnavailable {
			log.Printf("watchdog: target recovered from unavailable (now %q) while ON — re-issuing play", state)
			if err := w.ctrl.Replay(ctx); err != nil {
				log.Printf("watchdog: replay failed: %v", err)
			}
		}
	}
}
