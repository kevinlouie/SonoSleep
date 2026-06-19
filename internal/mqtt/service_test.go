package mqtt

import (
	"context"
	"sync"
	"testing"

	"github.com/kevin/ha-white-noise-sonos/internal/control"
)

// fakeBroker records publishes and stores subscription handlers so tests can
// drive command topics without a live broker.
type fakeBroker struct {
	mu        sync.Mutex
	published map[string]string // topic → last payload
	retained  map[string]bool
	handlers  map[string]func([]byte)
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

// fakePlayer records HA calls, satisfying control.Player.
type fakePlayer struct {
	mu         sync.Mutex
	plays      []play
	stops      int
	volumeSets []int
}

type play struct {
	preset string
	volume int
}

func (p *fakePlayer) PlayMedia(_ context.Context, preset string, volume int) error {
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

// newTestService builds a Service over a real control.State (the production
// Controller) and a fake player + broker. This exercises the true command→action
// path end to end without a network.
func newTestService(t *testing.T) (*Service, *fakeBroker, *fakePlayer) {
	t.Helper()
	player := &fakePlayer{}
	st := control.New(player, "brown", 80)
	broker := newFakeBroker()
	svc := NewService(broker, st)
	if err := svc.Subscribe(context.Background()); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return svc, broker, player
}

func TestPowerOnPlays(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(powerCommandTopic, "ON")

	if len(player.plays) != 1 {
		t.Fatalf("plays = %d, want 1", len(player.plays))
	}
	if player.plays[0] != (play{"brown", 80}) {
		t.Errorf("play = %+v, want {brown 80}", player.plays[0])
	}
	if got := broker.last(powerStateTopic); got != "ON" {
		t.Errorf("power state = %q, want ON", got)
	}
}

func TestPowerOffStops(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(powerCommandTopic, "ON")
	broker.send(powerCommandTopic, "OFF")

	if player.stops != 1 {
		t.Errorf("stops = %d, want 1", player.stops)
	}
	if got := broker.last(powerStateTopic); got != "OFF" {
		t.Errorf("power state = %q, want OFF", got)
	}
}

func TestPresetReplaysWhenOn(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(powerCommandTopic, "ON") // play #1 (brown)
	broker.send(presetCommandTopic, "pink")

	if len(player.plays) != 2 {
		t.Fatalf("plays = %d, want 2 (on + preset re-play)", len(player.plays))
	}
	if player.plays[1].preset != "pink" {
		t.Errorf("re-play preset = %q, want pink", player.plays[1].preset)
	}
	if got := broker.last(presetStateTopic); got != "pink" {
		t.Errorf("preset state = %q, want pink", got)
	}
}

func TestPresetNoReplayWhenOff(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(presetCommandTopic, "white")

	if len(player.plays) != 0 {
		t.Errorf("plays = %d, want 0 (switch is OFF)", len(player.plays))
	}
	if got := broker.last(presetStateTopic); got != "white" {
		t.Errorf("preset state = %q, want white", got)
	}
}

func TestInvalidPresetRejected(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(presetCommandTopic, "purple")

	if len(player.plays) != 0 {
		t.Errorf("plays = %d, want 0", len(player.plays))
	}
	// State unchanged → republished as the default (brown).
	if got := broker.last(presetStateTopic); got != "brown" {
		t.Errorf("preset state = %q, want brown (unchanged)", got)
	}
}

func TestVolumeSetsAndPublishes(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(volumeCommandTopic, "42")

	if len(player.volumeSets) != 1 || player.volumeSets[0] != 42 {
		t.Errorf("volumeSets = %v, want [42]", player.volumeSets)
	}
	if got := broker.last(volumeStateTopic); got != "42" {
		t.Errorf("volume state = %q, want 42", got)
	}
}

func TestVolumeClampedAbove100(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(volumeCommandTopic, "150")

	if len(player.volumeSets) != 1 || player.volumeSets[0] != 100 {
		t.Errorf("volumeSets = %v, want [100]", player.volumeSets)
	}
	if got := broker.last(volumeStateTopic); got != "100" {
		t.Errorf("volume state = %q, want 100", got)
	}
}

func TestNonIntegerVolumeIgnored(t *testing.T) {
	_, broker, player := newTestService(t)
	broker.send(volumeCommandTopic, "loud")

	if len(player.volumeSets) != 0 {
		t.Errorf("volumeSets = %v, want none", player.volumeSets)
	}
	if got := broker.last(volumeStateTopic); got != "80" {
		t.Errorf("volume state = %q, want 80 (unchanged)", got)
	}
}

func TestOnConnectPublishesDiscoveryAvailabilityAndState(t *testing.T) {
	svc, broker, _ := newTestService(t)
	if err := svc.OnConnect(context.Background()); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}
	// Discovery configs (retained).
	for _, topic := range []string{powerDiscoveryTopic, presetDiscoveryTopic, volumeDiscoveryTopic} {
		if broker.last(topic) == "" {
			t.Errorf("discovery topic %s not published", topic)
		}
		if !broker.retained[topic] {
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

func TestReconcileReassertsAuthoritativeState(t *testing.T) {
	// On reconnect, OnConnect must re-assert play_media from in-memory control
	// state (preset/volume), not from stale retained values. Set ON with pink/55,
	// then OnConnect should issue play_media(pink, 55).
	player := &fakePlayer{}
	st := control.New(player, "pink", 55)
	if err := st.SetOn(context.Background(), true); err != nil {
		t.Fatalf("set on: %v", err)
	}
	playsBefore := len(player.plays)

	broker := newFakeBroker()
	svc := NewService(broker, st)
	if err := svc.OnConnect(context.Background()); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}
	if len(player.plays) <= playsBefore {
		t.Errorf("reconcile did not re-assert play_media")
	}
	last := player.plays[len(player.plays)-1]
	if last != (play{"pink", 55}) {
		t.Errorf("reasserted play = %+v, want {pink 55}", last)
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
	if !broker.retained[AvailabilityTopic] {
		t.Errorf("offline availability should be retained")
	}
}
