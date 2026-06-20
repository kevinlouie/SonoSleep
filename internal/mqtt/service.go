package mqtt

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/kevin/ha-white-noise-sonos/internal/control"
)

// Broker is the minimal transport the Service needs. It is satisfied by the paho
// adapter (see paho.go) and by a fake in tests, so the command-mapping, state-
// publishing and reconcile logic run without a live broker.
type Broker interface {
	// Publish sends payload to topic. retained marks it as a retained message.
	Publish(topic string, payload []byte, retained bool) error
	// Subscribe registers handler for topic. handler is invoked per message.
	Subscribe(topic string, handler func(payload []byte)) error
}

// Controller is the control surface the MQTT command handlers drive. It is
// satisfied by *control.State.
type Controller interface {
	SetOn(ctx context.Context, on bool) error
	SetPreset(ctx context.Context, preset string) error
	SetVolume(ctx context.Context, level int) error
	Reassert(ctx context.Context) error
	Snapshot() control.Snapshot
}

// Service wires the broker to the control state: it publishes discovery,
// subscribes to command topics, applies commands to the control state, and
// publishes resulting state. Construct with NewService.
type Service struct {
	broker Broker
	ctrl   Controller
}

// NewService returns a Service over broker and ctrl.
func NewService(broker Broker, ctrl Controller) *Service {
	return &Service{broker: broker, ctrl: ctrl}
}

// Subscribe registers the three command-topic handlers. Call once after connect.
func (s *Service) Subscribe(ctx context.Context) error {
	if err := s.broker.Subscribe(powerCommandTopic, func(p []byte) { s.onPower(ctx, p) }); err != nil {
		return err
	}
	if err := s.broker.Subscribe(presetCommandTopic, func(p []byte) { s.onPreset(ctx, p) }); err != nil {
		return err
	}
	if err := s.broker.Subscribe(volumeCommandTopic, func(p []byte) { s.onVolume(ctx, p) }); err != nil {
		return err
	}
	return nil
}

// onPower maps ON/OFF → control.SetOn, then republishes state.
func (s *Service) onPower(ctx context.Context, payload []byte) {
	on := strings.EqualFold(strings.TrimSpace(string(payload)), payloadOn)
	if err := s.ctrl.SetOn(ctx, on); err != nil {
		slog.Error("mqtt: power command failed", "on", on, "err", err)
	}
	s.PublishState()
}

// onPreset maps a preset name → control.SetPreset (re-plays if ON), then
// republishes state.
func (s *Service) onPreset(ctx context.Context, payload []byte) {
	preset := strings.ToLower(strings.TrimSpace(string(payload)))
	if err := s.ctrl.SetPreset(ctx, preset); err != nil {
		slog.Warn("mqtt: preset command rejected", "preset", preset, "err", err)
		// State unchanged on rejection; re-publish so HA reverts to the truth.
	}
	s.PublishState()
}

// onVolume maps a 0–100 integer → control.SetVolume, then republishes state.
func (s *Service) onVolume(ctx context.Context, payload []byte) {
	v, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil {
		slog.Warn("mqtt: volume command is not an integer", "payload", string(payload), "err", err)
		s.PublishState()
		return
	}
	if err := s.ctrl.SetVolume(ctx, v); err != nil {
		slog.Error("mqtt: volume command failed", "volume", v, "err", err)
	}
	s.PublishState()
}

// PublishDiscovery publishes the three retained discovery configs.
func (s *Service) PublishDiscovery() error {
	for _, d := range discoveryDocs() {
		if err := s.broker.Publish(d.Topic, d.Payload, true); err != nil {
			return err
		}
	}
	return nil
}

// PublishAvailable publishes "online" (retained) to the availability topic.
func (s *Service) PublishAvailable() error {
	return s.broker.Publish(AvailabilityTopic, []byte(payloadOnline), true)
}

// PublishOffline publishes "offline" (retained) to the availability topic. Used
// on graceful shutdown; the LWT covers ungraceful disconnects.
func (s *Service) PublishOffline() error {
	return s.broker.Publish(AvailabilityTopic, []byte(payloadOffline), true)
}

// PublishState publishes the current control state to all three state topics
// (retained), so HA stays in sync after every change.
func (s *Service) PublishState() {
	snap := s.ctrl.Snapshot()
	power := payloadOff
	if snap.On {
		power = payloadOn
	}
	publishOne(s.broker, powerStateTopic, power)
	publishOne(s.broker, presetStateTopic, snap.Preset)
	publishOne(s.broker, volumeStateTopic, strconv.Itoa(snap.Volume))
}

// OnConnect performs the full on-connect / on-reconnect sequence: re-publish
// discovery, mark available, re-assert the authoritative state to Home Assistant
// (NOT stale retained values), then publish current state. Safe to call on every
// (re)connect.
func (s *Service) OnConnect(ctx context.Context) error {
	if err := s.PublishDiscovery(); err != nil {
		return err
	}
	if err := s.PublishAvailable(); err != nil {
		return err
	}
	if err := s.ctrl.Reassert(ctx); err != nil {
		// Reassert failure (e.g. Sonos unavailable) is non-fatal: log and still
		// publish state so HA reflects the intended control state.
		slog.Warn("mqtt: reconcile reassert failed", "err", err)
	}
	s.PublishState()
	return nil
}

func publishOne(b Broker, topic, payload string) {
	if err := b.Publish(topic, []byte(payload), true); err != nil {
		slog.Error("mqtt: publish failed", "topic", topic, "err", err)
	}
}
