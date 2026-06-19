package ha

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeState is an injectable stateGetter returning a scripted sequence of states.
type fakeState struct {
	states []string
	i      int
	err    error
}

func (f *fakeState) GetState(context.Context) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	s := f.states[f.i]
	if f.i < len(f.states)-1 {
		f.i++
	}
	return s, nil
}

// fakeCtrl is a test Controller with knobs for on/off and suppression and a
// replay counter.
type fakeCtrl struct {
	on         bool
	suppressed time.Time
	replays    int
	replayErr  error
}

func (c *fakeCtrl) IsOn() bool                   { return c.on }
func (c *fakeCtrl) SuppressedUntil() time.Time   { return c.suppressed }
func (c *fakeCtrl) Replay(context.Context) error { c.replays++; return c.replayErr }

func newWD(states []string, ctrl *fakeCtrl) *Watchdog {
	w := NewWatchdog(&fakeState{states: states}, ctrl, time.Hour)
	return w
}

func TestWatchdog_IdleWhileOn_Replays(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"idle"}, ctrl)
	w.tick(context.Background())
	if ctrl.replays != 1 {
		t.Fatalf("replays = %d, want 1", ctrl.replays)
	}
}

func TestWatchdog_PausedWhileOn_Replays(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"paused"}, ctrl)
	w.tick(context.Background())
	if ctrl.replays != 1 {
		t.Fatalf("replays = %d, want 1", ctrl.replays)
	}
}

func TestWatchdog_IdleWhileOff_NoReplay(t *testing.T) {
	ctrl := &fakeCtrl{on: false}
	w := newWD([]string{"idle"}, ctrl)
	w.tick(context.Background())
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (switch off)", ctrl.replays)
	}
}

func TestWatchdog_Suppressed_NoReplay(t *testing.T) {
	ctrl := &fakeCtrl{on: true, suppressed: time.Now().Add(time.Minute)}
	w := newWD([]string{"idle"}, ctrl)
	w.now = func() time.Time { return time.Now() }
	w.tick(context.Background())
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (suppressed)", ctrl.replays)
	}
}

func TestWatchdog_SuppressionExpired_Replays(t *testing.T) {
	ctrl := &fakeCtrl{on: true, suppressed: time.Now().Add(-time.Minute)}
	w := newWD([]string{"idle"}, ctrl)
	w.tick(context.Background())
	if ctrl.replays != 1 {
		t.Fatalf("replays = %d, want 1 (suppression expired)", ctrl.replays)
	}
}

func TestWatchdog_Playing_NoReplay(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"playing"}, ctrl)
	w.tick(context.Background())
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (already playing)", ctrl.replays)
	}
}

func TestWatchdog_RecoverFromUnavailable_Replays(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	// First tick sees unavailable (no replay), second sees buffering/on → recovery.
	w := newWD([]string{"unavailable", "buffering"}, ctrl)
	w.tick(context.Background())
	if ctrl.replays != 0 {
		t.Fatalf("after unavailable: replays = %d, want 0", ctrl.replays)
	}
	w.tick(context.Background())
	if ctrl.replays != 1 {
		t.Fatalf("after recovery: replays = %d, want 1", ctrl.replays)
	}
}

func TestWatchdog_StillUnavailable_NoReplay(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"unavailable"}, ctrl)
	w.tick(context.Background())
	w.tick(context.Background())
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (still unavailable)", ctrl.replays)
	}
}

func TestWatchdog_GetStateError_NoReplay(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := NewWatchdog(&fakeState{err: errors.New("boom")}, ctrl, time.Hour)
	w.tick(context.Background())
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (get_state failed)", ctrl.replays)
	}
}

func TestWatchdog_RunCancels(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := NewWatchdog(&fakeState{states: []string{"playing"}}, ctrl, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestNewWatchdog_DefaultInterval(t *testing.T) {
	w := NewWatchdog(&fakeState{states: []string{"idle"}}, &fakeCtrl{}, 0)
	if w.interval != defaultWatchdogInterval {
		t.Fatalf("interval = %v, want %v", w.interval, defaultWatchdogInterval)
	}
}
