package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kevin/ha-white-noise-sonos/internal/ha"
)

// fakePlayer records HA calls. Knobs: state is what GetState reports; playBlocks
// makes PlayMedia wait for ctx cancellation (like PlayMedia backing off on an
// unavailable target); playGate, when set, makes PlayMedia signal playEntered
// and then wait for the gate regardless of ctx (a POST already on the wire);
// stopFails/volumeFails make the first N calls fail.
type fakePlayer struct {
	mu          sync.Mutex
	calls       []string // "play", "stop", "volume", in order
	plays       []play
	stops       int
	volumes     []int
	state       string
	playBlocks  bool
	playGate    chan struct{}
	playEntered chan struct{}
	// stateGate, when set, makes GetState signal stateEntered and wait for the
	// gate (ignoring ctx) before answering.
	stateGate    chan struct{}
	stateEntered chan struct{}
	playCtxErrs  int // PlayMedia calls that returned because ctx was cancelled
	stopCalls    int // MediaStop attempts, failed or not
	stopFails    int
	volumeFails  int
	playErr      error // returned by every PlayMedia call, recording nothing
	playCalls    int   // PlayMedia attempts, of any outcome
}

type play struct {
	preset string
	volume int
}

func (p *fakePlayer) PlayMedia(ctx context.Context, preset string, volume int) error {
	p.mu.Lock()
	p.playCalls++
	block, gate, playErr := p.playBlocks, p.playGate, p.playErr
	p.mu.Unlock()
	if playErr != nil {
		return playErr
	}
	if gate != nil {
		p.playEntered <- struct{}{}
		<-gate
		if ctx.Err() != nil {
			p.mu.Lock()
			p.playCtxErrs++ // the op was cancelled while its POST was "on the wire"
			p.mu.Unlock()
		}
	}
	if block {
		<-ctx.Done()
		p.mu.Lock()
		p.playCtxErrs++
		p.mu.Unlock()
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "play")
	p.plays = append(p.plays, play{preset, volume})
	return nil
}

func (p *fakePlayer) VolumeSet(_ context.Context, level int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.volumeFails > 0 {
		p.volumeFails--
		return errors.New("volume_set boom")
	}
	p.calls = append(p.calls, "volume")
	p.volumes = append(p.volumes, level)
	return nil
}

func (p *fakePlayer) MediaStop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopCalls++
	if p.stopFails > 0 {
		p.stopFails--
		return errors.New("media_stop boom")
	}
	p.calls = append(p.calls, "stop")
	p.stops++
	return nil
}

func (p *fakePlayer) GetState(context.Context) (string, error) {
	p.mu.Lock()
	gate := p.stateGate
	p.mu.Unlock()
	if gate != nil {
		p.stateEntered <- struct{}{}
		<-gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "" {
		return "idle", nil
	}
	return p.state, nil
}

func (p *fakePlayer) snapshot() (plays []play, stops int, volumes []int, calls []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]play(nil), p.plays...), p.stops, append([]int(nil), p.volumes...), append([]string(nil), p.calls...)
}

// start runs s's reconciler until the test ends, with fast retries, and waits
// for it to exit on cleanup so no goroutine outlives the test.
func start(t *testing.T, s *State) {
	t.Helper()
	startWithRetry(t, s, time.Millisecond, 5*time.Millisecond)
}

func startWithRetry(t *testing.T, s *State, base, maxDelay time.Duration) {
	t.Helper()
	s.retryBase = base
	s.retryMax = maxDelay
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// waitFor polls cond until true or fails the test after a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// idle reports whether the reconciler has nothing pending or in flight.
func (s *State) idle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.powerWant == s.powerDone && s.volWant == s.volDone && s.cancelOp == nil
}

func TestSetOnPlaysAndSetsState(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetOn(true)
	if !s.IsOn() {
		t.Error("IsOn = false after SetOn(true)")
	}
	waitFor(t, "play", func() bool { plays, _, _, _ := p.snapshot(); return len(plays) == 1 })
	if plays, _, _, _ := p.snapshot(); plays[0] != (play{"brown", 80}) {
		t.Errorf("plays = %v", plays)
	}
}

func TestSetOnDoesNotCallHASynchronously(t *testing.T) {
	// No reconciler running: recording intent must not touch HA at all.
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	s.SetOn(true)
	s.SetVolume(10)
	_ = s.SetPreset("pink")
	s.SetOn(false)
	if _, _, _, calls := p.snapshot(); len(calls) != 0 {
		t.Errorf("HA calls without reconciler = %v, want none", calls)
	}
}

func TestSetOffStops(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetOn(true)
	waitFor(t, "idle after on", s.idle)
	s.SetOn(false)
	if s.IsOn() {
		t.Error("IsOn = true after SetOn(false)")
	}
	waitFor(t, "stop", func() bool { _, stops, _, _ := p.snapshot(); return stops == 1 })
}

func TestPlaySetsSuppressWindow(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	fixed := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return fixed }
	start(t, s)
	s.SetOn(true)
	waitFor(t, "play", s.idle)
	// Watchdog must stand down for suppressWindow after every play.
	if until := s.SuppressedUntil(); !until.Equal(fixed.Add(suppressWindow)) {
		t.Errorf("SuppressedUntil = %v, want %v", until, fixed.Add(suppressWindow))
	}
}

func TestOffCancelsInFlightPlay(t *testing.T) {
	// A play stuck in PlayMedia (e.g. backing off on an unavailable target) must
	// be abandoned by a later OFF, and must never complete after the stop.
	p := &fakePlayer{playBlocks: true}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetOn(true)
	waitFor(t, "play in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cancelOp != nil
	})
	s.SetOn(false)
	waitFor(t, "stop", func() bool { _, stops, _, _ := p.snapshot(); return stops == 1 })
	waitFor(t, "idle", s.idle)
	p.mu.Lock()
	cancelled := p.playCtxErrs
	p.mu.Unlock()
	if cancelled != 1 {
		t.Errorf("cancelled plays = %d, want 1", cancelled)
	}
	if plays, _, _, calls := p.snapshot(); len(plays) != 0 {
		t.Errorf("play issued from a stale intent: calls = %v", calls)
	}
}

func TestReplayNoOpWhenOff(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	start(t, s)
	s.Replay()
	time.Sleep(10 * time.Millisecond)
	if plays, _, _, _ := p.snapshot(); len(plays) != 0 {
		t.Errorf("Replay played while OFF: %v", plays)
	}
}

func TestReplayPlaysWhenOn(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "pink", 30)
	start(t, s)
	s.SetOn(true)
	waitFor(t, "first play", s.idle)
	s.Replay()
	waitFor(t, "replay", func() bool { plays, _, _, _ := p.snapshot(); return len(plays) == 2 })
	if plays, _, _, _ := p.snapshot(); plays[1] != (play{"pink", 30}) {
		t.Errorf("replay = %+v, want {pink 30}", plays[1])
	}
}

func TestReplayDoesNotRestartPendingPlay(t *testing.T) {
	p := &fakePlayer{playBlocks: true}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetOn(true)
	waitFor(t, "play in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cancelOp != nil
	})
	s.Replay()
	time.Sleep(10 * time.Millisecond)
	p.mu.Lock()
	cancelled := p.playCtxErrs
	p.mu.Unlock()
	if cancelled != 0 {
		t.Errorf("Replay cancelled the in-flight play (%d)", cancelled)
	}
}

func TestSetVolumeClampsAndSnapshots(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetVolume(200)
	if snap := s.Snapshot(); snap.Volume != 100 {
		t.Errorf("snapshot volume = %d, want 100", snap.Volume)
	}
	waitFor(t, "volume_set", func() bool { _, _, v, _ := p.snapshot(); return len(v) == 1 })
	if _, _, v, _ := p.snapshot(); v[0] != 100 {
		t.Errorf("volume_set = %d, want 100", v[0])
	}
}

func TestSetPresetRejectsInvalid(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	if err := s.SetPreset("purple"); err == nil {
		t.Error("expected error for invalid preset")
	}
	if snap := s.Snapshot(); snap.Preset != "brown" {
		t.Errorf("preset = %q, want unchanged brown", snap.Preset)
	}
}

func TestSetPresetReplaysOnlyWhenOnAndChanged(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	start(t, s)
	_ = s.SetPreset("white") // OFF: no play
	s.SetOn(true)
	waitFor(t, "play", s.idle)
	_ = s.SetPreset("white") // unchanged: no restart
	_ = s.SetPreset("pink")
	waitFor(t, "re-play", func() bool { plays, _, _, _ := p.snapshot(); return len(plays) == 2 })
	waitFor(t, "idle", s.idle)
	plays, _, _, _ := p.snapshot()
	if len(plays) != 2 || plays[0].preset != "white" || plays[1].preset != "pink" {
		t.Errorf("plays = %v, want [white pink]", plays)
	}
}

func TestFailedStopIsRetried(t *testing.T) {
	p := &fakePlayer{stopFails: 2}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetOn(false)
	waitFor(t, "stop after retries", func() bool { _, stops, _, _ := p.snapshot(); return stops == 1 })
	waitFor(t, "idle", s.idle)
}

func TestFailedOpBacksOff(t *testing.T) {
	// A persistently failing op must not be retried in a hot loop.
	p := &fakePlayer{stopFails: 1 << 30}
	s := New(p, "brown", 80)
	startWithRetry(t, s, 20*time.Millisecond, 20*time.Millisecond)
	s.SetOn(false)
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	n := p.stopCalls
	p.mu.Unlock()
	if n < 2 || n > 8 {
		t.Errorf("media_stop attempts in 100ms = %d, want a few (retried with backoff, not spinning)", n)
	}
}

func TestFailedVolumeIsRetried(t *testing.T) {
	p := &fakePlayer{volumeFails: 3}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetVolume(25)
	waitFor(t, "volume_set after retries", func() bool { _, _, v, _ := p.snapshot(); return len(v) == 1 && v[0] == 25 })
	waitFor(t, "idle", s.idle)
}

func TestPlayAppliesPendingVolume(t *testing.T) {
	// PlayMedia already sets the volume, so a volume change recorded before the
	// play must not cause an extra volume_set afterwards.
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	s.SetVolume(40)
	s.SetOn(true)
	start(t, s)
	waitFor(t, "idle", s.idle)
	plays, _, volumes, _ := p.snapshot()
	if len(plays) != 1 || plays[0].volume != 40 {
		t.Errorf("plays = %v, want one at volume 40", plays)
	}
	if len(volumes) != 0 {
		t.Errorf("extra volume_set calls = %v, want none", volumes)
	}
}

func TestReassertOffDoesNothing(t *testing.T) {
	p := &fakePlayer{state: "playing"}
	s := New(p, "brown", 80)
	start(t, s)
	s.Reassert()
	time.Sleep(10 * time.Millisecond)
	if _, _, _, calls := p.snapshot(); len(calls) != 0 {
		t.Errorf("Reassert while OFF made HA calls %v, want none (must not stop unrelated playback)", calls)
	}
}

func TestReassertOnAlreadyPlayingDoesNotRestart(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetOn(true)
	waitFor(t, "play", s.idle)
	p.mu.Lock()
	p.state = "playing"
	p.mu.Unlock()
	s.Reassert()
	s.SetOn(true) // ON while ON behaves the same way
	waitFor(t, "idle", s.idle)
	if plays, _, _, _ := p.snapshot(); len(plays) != 1 {
		t.Errorf("plays = %d, want 1 (healthy playback must not restart)", len(plays))
	}
}

func TestReassertOnNotPlayingReplays(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "pink", 55)
	start(t, s)
	s.SetOn(true)
	waitFor(t, "play", s.idle)
	p.mu.Lock()
	p.state = "idle"
	p.mu.Unlock()
	s.Reassert()
	waitFor(t, "re-play", func() bool { plays, _, _, _ := p.snapshot(); return len(plays) == 2 })
	if plays, _, _, _ := p.snapshot(); plays[1] != (play{"pink", 55}) {
		t.Errorf("reasserted play = %+v, want {pink 55}", plays[1])
	}
}

func TestVolumeChangeDoesNotRestartInFlightPlay(t *testing.T) {
	// A volume change while a play is in flight must not cancel it: exactly one
	// play_media, then a volume_set with the new level.
	gate := make(chan struct{})
	p := &fakePlayer{playGate: gate, playEntered: make(chan struct{}, 1)}
	s := New(p, "brown", 80)
	start(t, s)
	s.SetOn(true)
	<-p.playEntered
	s.SetVolume(30)
	s.SetVolume(35)
	close(gate)
	waitFor(t, "idle", s.idle)
	p.mu.Lock()
	cancelled := p.playCtxErrs
	p.mu.Unlock()
	if cancelled != 0 {
		t.Errorf("in-flight play was cancelled by a volume change")
	}
	plays, _, volumes, calls := p.snapshot()
	if len(plays) != 1 || plays[0].volume != 80 {
		t.Errorf("plays = %v, want exactly one (at the original volume 80)", plays)
	}
	if len(volumes) == 0 || volumes[len(volumes)-1] != 35 || calls[len(calls)-1] != "volume" {
		t.Errorf("calls = %v volumes = %v, want a trailing volume_set(35)", calls, volumes)
	}
}

func TestReplayUpgradesPendingEnsurePlay(t *testing.T) {
	// Reconnect queued "play only if not playing"; HA still (stale) says
	// playing. The watchdog has just seen it stopped, so its Replay must force
	// the play.
	p := &fakePlayer{state: "playing"}
	s := New(p, "brown", 80)
	s.on = true // ON, with no play pending yet (as after a completed play)
	s.Reassert()
	s.Replay()
	start(t, s)
	waitFor(t, "idle", s.idle)
	if plays, _, _, _ := p.snapshot(); len(plays) != 1 {
		t.Errorf("plays = %d, want 1 (Replay must upgrade the pending ensure-play)", len(plays))
	}
}

func TestReplayDuringEnsureGetStateForcesPlay(t *testing.T) {
	// The ensure-play is already in apply (GetState in flight) when the
	// watchdog's Replay arrives; GetState then reports a stale "playing". The
	// replay must still issue play_media.
	gate := make(chan struct{})
	p := &fakePlayer{state: "playing", stateGate: gate, stateEntered: make(chan struct{}, 1)}
	s := New(p, "brown", 80)
	s.on = true
	s.Reassert()
	start(t, s)
	<-p.stateEntered
	s.Replay()
	close(gate)
	waitFor(t, "idle", s.idle)
	if plays, _, _, _ := p.snapshot(); len(plays) != 1 {
		t.Errorf("plays = %d, want 1 (replay swallowed by stale ensure-play)", len(plays))
	}
}

func TestNeedsFinalStop(t *testing.T) {
	p := &fakePlayer{stopFails: 1 << 30}
	s := New(p, "brown", 80)
	if s.NeedsFinalStop() {
		t.Error("fresh state: NeedsFinalStop = true, want false (never played)")
	}
	startWithRetry(t, s, time.Hour, time.Hour) // one stop attempt, then a long wait
	s.SetOn(true)
	waitFor(t, "play", s.idle)
	s.SetOn(false)
	waitFor(t, "failed stop", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.stopCalls == 1
	})
	if !s.NeedsFinalStop() {
		t.Error("OFF with stop not landed: NeedsFinalStop = false, want true")
	}
	p.mu.Lock()
	p.stopFails = 0
	p.mu.Unlock()
	s.SetOn(false) // fresh OFF intent interrupts the retry wait
	waitFor(t, "idle", s.idle)
	if s.NeedsFinalStop() {
		t.Error("after a successful stop: NeedsFinalStop = true, want false")
	}
}

func TestNeedsFinalStopAfterPlayWithUnknownOutcome(t *testing.T) {
	// Shutdown with an OFF pending after a play that failed: only a play whose
	// outcome is unknown (HA may have started it) needs the final media_stop.
	cases := []struct {
		name    string
		playErr error
		want    bool
	}{
		{"outcome unknown", fmt.Errorf("ha: play_media: %w", ha.ErrOutcomeUnknown), true},
		{"definite failure", errors.New("ha: play_media: status 500"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePlayer{playErr: tc.playErr, stopFails: 1 << 30}
			s := New(p, "brown", 80)
			s.retryBase, s.retryMax = time.Hour, time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { s.Run(ctx); close(done) }()
			s.SetOn(true)
			waitFor(t, "failed play finished", func() bool {
				p.mu.Lock()
				attempted := p.playCalls == 1
				p.mu.Unlock()
				s.mu.Lock()
				defer s.mu.Unlock()
				return attempted && s.cancelOp == nil
			})
			s.SetOn(false) // OFF pending; its stop keeps failing
			cancel()       // shutdown stops the reconciler
			<-done
			if got := s.NeedsFinalStop(); got != tc.want {
				t.Errorf("NeedsFinalStop = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNeedsFinalStopFalseWhenOffAndNeverPlayed(t *testing.T) {
	p := &fakePlayer{}
	s := New(p, "brown", 80)
	start(t, s)
	_ = s.SetPreset("pink")
	s.SetVolume(20)
	waitFor(t, "idle", s.idle)
	if s.NeedsFinalStop() {
		t.Error("NeedsFinalStop = true for a service that never played, want false")
	}
}

func TestOffDuringPlayPOSTStopsAfterIt(t *testing.T) {
	// End to end with the real HA client: OFF arrives while play_media is on
	// the wire. The play must complete (not be aborted and possibly applied by
	// HA later), and media_stop must reach HA only after it.
	var mu sync.Mutex
	var order []string
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/states/media_player.bedroom":
			_, _ = io.WriteString(w, `{"state":"idle"}`)
			return
		case "/api/services/media_player/play_media":
			close(entered)
			<-release
			mu.Lock()
			order = append(order, "play_media done")
			mu.Unlock()
		case "/api/services/media_player/media_stop":
			mu.Lock()
			order = append(order, "media_stop")
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := ha.New(srv.URL, "tok", "media_player.bedroom", "http://host:8099")
	s := New(client, "brown", 80)
	start(t, s)
	s.SetOn(true)
	<-entered
	s.SetOn(false)
	time.Sleep(20 * time.Millisecond) // give a wrongly-unordered stop time to arrive
	close(release)
	waitFor(t, "idle", s.idle)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "play_media done" || order[1] != "media_stop" {
		t.Errorf("HA saw %v, want [play_media done media_stop]", order)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	p := &fakePlayer{playBlocks: true}
	s := New(p, "brown", 80)
	s.SetOn(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {7, time.Minute}, {100, time.Minute}}
	for _, tc := range cases {
		if got := retryDelay(tc.failures, time.Second, time.Minute); got != tc.want {
			t.Errorf("retryDelay(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
}
