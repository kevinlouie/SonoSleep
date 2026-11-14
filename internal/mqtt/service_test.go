package mqtt

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kevin/ha-white-noise-sonos/internal/control"
)

// fakeBroker records publishes and stores subscription handlers so tests can
// drive command topics without a live broker.
type fakeBroker struct {
	mu        sync.Mutex
	published map[string]string // topic → last payload
	retained  map[string]bool
	handlers  map[string]func([]byte)
	// block, when non-nil, makes Publish wait on it (a slow/unacked broker).
	block chan struct{}
}

func newFakeBroker() *fakeBroker {
	return &fakeBroker{
		published: map[string]string{},
		retained:  map[string]bool{},
		handlers:  map[string]func([]byte){},
	}
}

func (b *fakeBroker) Publish(topic string, payload []byte, retained bool) error {
	b.mu.Lock()
	block := b.block
	b.mu.Unlock()
	if block != nil {
		<-block
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published[topic] = string(payload)
	b.retained[topic] = retained
	return nil
}

func (b *fakeBroker) Subscribe(topic string, handler func([]byte)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[topic] = handler
	return nil
}

func (b *fakeBroker) send(topic, payload string) {
	b.mu.Lock()
	h := b.handlers[topic]
	b.mu.Unlock()
	if h != nil {
		h([]byte(payload))
	}
}

func (b *fakeBroker) last(topic string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.published[topic]
}

func (b *fakeBroker) isRetained(topic string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.retained[topic]
}

// fakePlayer records HA calls, satisfying control.Player. state is what
// GetState reports (default idle); playBlocks makes PlayMedia hang until its
// context is cancelled, like a play backing off on an unavailable Sonos.
type fakePlayer struct {
	mu         sync.Mutex
	plays      []play
	stops      int
	volumeSets []int
	state      string
	playBlocks bool
}

type play struct {
	preset string
	volume int
}

func (p *fakePlayer) PlayMedia(ctx context.Context, preset string, volume int) error {
	p.mu.Lock()
	block := p.playBlocks
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plays = append(p.plays, play{preset, volume})
	return nil
}

func (p *fakePlayer) VolumeSet(_ context.Context, level int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volumeSets = append(p.volumeSets, level)
	return nil
}

func (p *fakePlayer) MediaStop(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops++
	return nil
}

func (p *fakePlayer) GetState(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == "" {
		return "idle", nil
	}
	return p.state, nil
}

func (p *fakePlayer) counts() (plays []play, stops int, volumeSets []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]play(nil), p.plays...), p.stops, append([]int(nil), p.volumeSets...)
}

// run starts fn(ctx) in a goroutine for the rest of the test and waits for it to
// return on cleanup, so nothing leaks past the test.
func run(t *testing.T, fn func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fn(ctx); close(done) }()
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

// newTestService builds a Service over a real control.State (the production
// Controller) and a fake player + broker, with the reconciler and the state
// publisher running. This exercises the true command→action path end to end
// without a network.
func newTestService(t *testing.T) (*Service, *fakeBroker, *fakePlayer) {
	t.Helper()
	player := &fakePlayer{}
	st := control.New(player, "brown", 80)
	broker := newFakeBroker()
	svc := NewService(broker, st)
	if err := svc.Subscribe(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	run(t, st.Run)
	run(t, svc.Run)
	return svc, broker, player
}

func TestPowerOnPlays(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(powerCommandTopic, "ON")

	waitFor(t, "play", func() bool { plays, _, _ := player.counts(); return len(plays) == 1 })
	if plays, _, _ := player.counts(); plays[0] != (play{"brown", 80}) {
		t.Errorf("play = %+v, want {brown 80}", plays[0])
	}
	waitFor(t, "power state ON", func() bool { return broker.last(powerStateTopic) == "ON" })
}

func TestPowerOffStops(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(powerCommandTopic, "ON")
	broker.send(powerCommandTopic, "OFF")

	waitFor(t, "stop", func() bool { _, stops, _ := player.counts(); return stops == 1 })
	waitFor(t, "power state OFF", func() bool { return broker.last(powerStateTopic) == "OFF" })
}

func TestCommandHandlersDoNotBlock(t *testing.T) {
	// The Sonos is unavailable (play hangs) and the broker never acks a
	// publish: handlers on paho's router goroutine must still return at once,
	// and OFF must still win over the stuck ON.
	_, broker, player := newTestService(t)
	player.mu.Lock()
	player.playBlocks = true
	player.mu.Unlock()
	block := make(chan struct{})
	broker.mu.Lock()
	broker.block = block
	broker.mu.Unlock()
	defer close(block) // release the publisher so cleanup can stop it

	done := make(chan struct{})
	go func() {
		broker.send(powerCommandTopic, "ON")
		broker.send(volumeCommandTopic, "30")
		broker.send(presetCommandTopic, "pink")
		broker.send(powerCommandTopic, "OFF")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("command handlers blocked")
	}
	waitFor(t, "stop despite stuck play", func() bool { _, stops, _ := player.counts(); return stops == 1 })
}

func TestPresetReplaysWhenOn(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(powerCommandTopic, "ON") // play #1 (brown)
	waitFor(t, "play #1", func() bool { plays, _, _ := player.counts(); return len(plays) == 1 })
	broker.send(presetCommandTopic, "pink")

	waitFor(t, "re-play", func() bool { plays, _, _ := player.counts(); return len(plays) == 2 })
	if plays, _, _ := player.counts(); plays[1].preset != "pink" {
		t.Errorf("re-play preset = %q, want pink", plays[1].preset)
	}
	waitFor(t, "preset state pink", func() bool { return broker.last(presetStateTopic) == "pink" })
}

func TestPresetNoReplayWhenOff(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(presetCommandTopic, "white")

	waitFor(t, "preset state white", func() bool { return broker.last(presetStateTopic) == "white" })
	if plays, _, _ := player.counts(); len(plays) != 0 {
		t.Errorf("plays = %d, want 0 (switch is OFF)", len(plays))
	}
}

func TestInvalidPresetRejected(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(presetCommandTopic, "purple")

	// State unchanged → republished as the default (brown).
	waitFor(t, "preset state brown", func() bool { return broker.last(presetStateTopic) == "brown" })
	if plays, _, _ := player.counts(); len(plays) != 0 {
		t.Errorf("plays = %d, want 0", len(plays))
	}
}

func TestVolumeSetsAndPublishes(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(volumeCommandTopic, "42")

	waitFor(t, "volume_set", func() bool { _, _, v := player.counts(); return len(v) == 1 })
	if _, _, v := player.counts(); v[0] != 42 {
		t.Errorf("volumeSets = %v, want [42]", v)
	}
	waitFor(t, "volume state 42", func() bool { return broker.last(volumeStateTopic) == "42" })
}

func TestVolumeClampedAbove100(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(volumeCommandTopic, "150")

	waitFor(t, "volume_set", func() bool { _, _, v := player.counts(); return len(v) == 1 })
	if _, _, v := player.counts(); v[0] != 100 {
		t.Errorf("volumeSets = %v, want [100]", v)
	}
	waitFor(t, "volume state 100", func() bool { return broker.last(volumeStateTopic) == "100" })
}

func TestNonIntegerVolumeIgnored(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(volumeCommandTopic, "loud")

	waitFor(t, "volume state 80", func() bool { return broker.last(volumeStateTopic) == "80" })
	if _, _, v := player.counts(); len(v) != 0 {
		t.Errorf("volumeSets = %v, want none", v)
	}
}

func TestOnConnectPublishesDiscoveryAvailabilityAndState(t *testing.T) {
	svc, broker, _ := newTestService(t)
	if err := svc.OnConnect(); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}
	waitFor(t, "state published", func() bool { return broker.last(volumeStateTopic) != "" })
	// Discovery configs (retained).
	for _, topic := range []string{powerDiscoveryTopic, presetDiscoveryTopic, volumeDiscoveryTopic} {
		if broker.last(topic) == "" {
			t.Errorf("discovery topic %s not published", topic)
		}
		if !broker.isRetained(topic) {
			t.Errorf("discovery topic %s should be retained", topic)
		}
	}
	if broker.last(AvailabilityTopic) != payloadOnline {
		t.Errorf("availability = %q, want online", broker.last(AvailabilityTopic))
	}
	// State topics reflect defaults (OFF / brown / 80).
	if broker.last(powerStateTopic) != "OFF" {
		t.Errorf("power state = %q, want OFF", broker.last(powerStateTopic))
	}
	if broker.last(presetStateTopic) != "brown" {
		t.Errorf("preset state = %q, want brown", broker.last(presetStateTopic))
	}
	if broker.last(volumeStateTopic) != "80" {
		t.Errorf("volume state = %q, want 80", broker.last(volumeStateTopic))
	}
}

func TestOnConnectLeavesStatePublishingToRun(t *testing.T) {
	// Run must be the only writer of the state topics, so a stale snapshot
	// from a reconnect can't overwrite newer state published for a command.
	player := &fakePlayer{}
	st := control.New(player, "brown", 80)
	broker := newFakeBroker()
	svc := NewService(broker, st)
	if err := svc.OnConnect(); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}
	if broker.last(AvailabilityTopic) != payloadOnline {
		t.Errorf("availability = %q, want online (published directly)", broker.last(AvailabilityTopic))
	}
	for _, topic := range []string{powerStateTopic, presetStateTopic, volumeStateTopic} {
		if got := broker.last(topic); got != "" {
			t.Errorf("OnConnect published %s = %q itself, want it left to Run", topic, got)
		}
	}
	run(t, svc.Run)
	waitFor(t, "Run publishes the requested state", func() bool { return broker.last(powerStateTopic) == "OFF" })
}

func TestOnConnectWhileOffLeavesSpeakerAlone(t *testing.T) {
	// Container start / broker blip with the switch OFF must not media_stop
	// whatever unrelated music the Sonos is playing.
	svc, _, player := newTestService(t)
	player.mu.Lock()
	player.state = "playing"
	player.mu.Unlock()
	if err := svc.OnConnect(); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	plays, stops, volumes := player.counts()
	if len(plays) != 0 || stops != 0 || len(volumes) != 0 {
		t.Errorf("HA calls on connect while OFF: plays=%v stops=%d volumes=%v, want none", plays, stops, volumes)
	}
}

func TestReconcileReassertsAuthoritativeState(t *testing.T) {
	// On reconnect while ON and the target not playing, OnConnect must re-play
	// from in-memory control state (preset/volume), not stale retained values.
	player := &fakePlayer{}
	st := control.New(player, "pink", 55)
	run(t, st.Run)
	st.SetOn(true)
	// Wait until the reconciler has recorded the play as done (it sets the
	// suppress window in the same step), not just until the fake saw it:
	// otherwise the reassert below can coalesce into the still-pending op.
	waitFor(t, "initial play done", func() bool { return !st.SuppressedUntil().IsZero() })

	broker := newFakeBroker()
	svc := NewService(broker, st)
	if err := svc.OnConnect(); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}
	waitFor(t, "reasserted play", func() bool { plays, _, _ := player.counts(); return len(plays) == 2 })
	if plays, _, _ := player.counts(); plays[1] != (play{"pink", 55}) {
		t.Errorf("reasserted play = %+v, want {pink 55}", plays[1])
	}
}

func TestReconcileDoesNotRestartHealthyPlayback(t *testing.T) {
	player := &fakePlayer{}
	st := control.New(player, "pink", 55)
	run(t, st.Run)
	st.SetOn(true)
	// Wait until the reconciler has recorded the play as done (it sets the
	// suppress window in the same step), not just until the fake saw it:
	// otherwise the reassert below can coalesce into the still-pending op.
	waitFor(t, "initial play done", func() bool { return !st.SuppressedUntil().IsZero() })
	player.mu.Lock()
	player.state = "playing"
	player.mu.Unlock()

	svc := NewService(newFakeBroker(), st)
	if err := svc.OnConnect(); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if plays, _, _ := player.counts(); len(plays) != 1 {
		t.Errorf("plays = %d, want 1 (already playing: no restart)", len(plays))
	}
}

func TestPublishOffline(t *testing.T) {
	svc, broker, _ := newTestService(t)
	if err := svc.PublishOffline(); err != nil {
		t.Fatalf("PublishOffline: %v", err)
	}
	if got := broker.last(AvailabilityTopic); got != payloadOffline {
		t.Errorf("availability = %q, want offline", got)
	}
	if !broker.isRetained(AvailabilityTopic) {
		t.Errorf("offline availability should be retained")
	}
}
