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
}

func (c *fakeCtrl) IsOn() bool                 { return c.on }
func (c *fakeCtrl) SuppressedUntil() time.Time { return c.suppressed }
func (c *fakeCtrl) Replay()                    { c.replays++ }

func newWD(states []string, ctrl *fakeCtrl) *Watchdog {
	w := NewWatchdog(&fakeState{states: states}, ctrl, time.Hour)
	return w
}

// ticks runs n watchdog polls.
func ticks(w *Watchdog, n int) {
	for i := 0; i < n; i++ {
		w.tick(context.Background())
	}
}

func TestWatchdog_IdleWhileOn_ReplaysAfterTwoPolls(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"idle"}, ctrl)
	w.tick(context.Background())
	if ctrl.replays != 0 {
		t.Fatalf("after 1 idle poll: replays = %d, want 0 (may still be buffering)", ctrl.replays)
	}
	w.tick(context.Background())
	if ctrl.replays != 1 {
		t.Fatalf("after 2 idle polls: replays = %d, want 1", ctrl.replays)
	}
}

func TestWatchdog_PausedWhileOn_ReplaysAfterTwoPolls(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"paused"}, ctrl)
	ticks(w, 2)
	if ctrl.replays != 1 {
		t.Fatalf("replays = %d, want 1", ctrl.replays)
	}
}

func TestWatchdog_IdleStreakResetByPlaying(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"idle", "playing", "idle"}, ctrl)
	ticks(w, 3)
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (idle polls not consecutive)", ctrl.replays)
	}
}

func TestWatchdog_IdleStreakResetBySuppression(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"idle"}, ctrl)
	w.tick(context.Background()) // streak 1
	ctrl.suppressed = time.Now().Add(time.Minute)
	w.tick(context.Background()) // suppressed: resets streak
	ctrl.suppressed = time.Time{}
	w.tick(context.Background()) // streak 1 again
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (suppression resets the idle streak)", ctrl.replays)
	}
	w.tick(context.Background())
	if ctrl.replays != 1 {
		t.Fatalf("replays = %d, want 1", ctrl.replays)
	}
}

func TestWatchdog_IdleWhileOff_NoReplay(t *testing.T) {
	ctrl := &fakeCtrl{on: false}
	w := newWD([]string{"idle"}, ctrl)
	ticks(w, 3)
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (switch off)", ctrl.replays)
	}
}

func TestWatchdog_Suppressed_NoReplay(t *testing.T) {
	ctrl := &fakeCtrl{on: true, suppressed: time.Now().Add(time.Minute)}
	w := newWD([]string{"idle"}, ctrl)
	w.now = func() time.Time { return time.Now() }
	ticks(w, 3)
	if ctrl.replays != 0 {
		t.Fatalf("replays = %d, want 0 (suppressed)", ctrl.replays)
	}
}

func TestWatchdog_SuppressionExpired_Replays(t *testing.T) {
	ctrl := &fakeCtrl{on: true, suppressed: time.Now().Add(-time.Minute)}
	w := newWD([]string{"idle"}, ctrl)
	ticks(w, 2)
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

func TestWatchdog_RecoverFromUnavailableToIdle_ReplaysImmediately(t *testing.T) {
	ctrl := &fakeCtrl{on: true}
	w := newWD([]string{"unavailable", "idle"}, ctrl)
	ticks(w, 2)
	if ctrl.replays != 1 {
		t.Fatalf("replays = %d, want 1 (recovery edge needs no second idle poll)", ctrl.replays)
	}
}

func TestWatchdog_RecoveryEdgeNotSwallowedBySuppression(t *testing.T) {
	ctrl := &fakeCtrl{on: true, suppressed: time.Now().Add(time.Minute)}
	w := newWD([]string{"unavailable", "idle"}, ctrl)
	ticks(w, 2)
	if ctrl.replays != 1 {
		t.Fatalf("replays = %d, want 1 (recovery edge must bypass the suppress window)", ctrl.replays)
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
