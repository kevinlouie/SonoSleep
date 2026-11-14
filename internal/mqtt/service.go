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
// satisfied by *control.State. Every method must return promptly: they only
// record intent, and the HA calls happen on the controller's reconciler.
type Controller interface {
	SetOn(on bool)
	SetPreset(preset string) error
	SetVolume(level int)
	Reassert()
	Snapshot() control.Snapshot
}

// Service wires the broker to the control state: it publishes discovery,
// subscribes to command topics, records commands in the control state, and
// publishes resulting state. Construct with NewService and start Run.
//
// Command handlers never block: paho's ordered router calls them synchronously
// (and its docs say they must not block), so they only record intent and ask
// Run to publish the new state. Run does the publishing (each QoS1 publish can
// wait up to publishTimeout) off the router goroutine.
type Service struct {
	broker Broker
	ctrl   Controller
	// pub (cap 1) is a coalesced "state changed, publish it" signal to Run.
	pub chan struct{}
}

// NewService returns a Service over broker and ctrl.
func NewService(broker Broker, ctrl Controller) *Service {
	return &Service{broker: broker, ctrl: ctrl, pub: make(chan struct{}, 1)}
}

// Run publishes the current state whenever a command handler asks for it,
// until ctx is cancelled. Bursts of commands coalesce into one publish of the
// latest state.
func (s *Service) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.pub:
			s.PublishState()
		}
	}
}

// requestPublish asks Run to publish the current state, without blocking.
func (s *Service) requestPublish() {
	select {
	case s.pub <- struct{}{}:
	default:
	}
}

// Subscribe registers the three command-topic handlers. Call on every connect.
func (s *Service) Subscribe() error {
	if err := s.broker.Subscribe(powerCommandTopic, s.onPower); err != nil {
		return err
	}
	if err := s.broker.Subscribe(presetCommandTopic, s.onPreset); err != nil {
		return err
	}
	if err := s.broker.Subscribe(volumeCommandTopic, s.onVolume); err != nil {
		return err
	}
	return nil
}

// onPower maps ON/OFF → control.SetOn, then requests a state publish.
func (s *Service) onPower(payload []byte) {
	on := strings.EqualFold(strings.TrimSpace(string(payload)), payloadOn)
	s.ctrl.SetOn(on)
	s.requestPublish()
}

// onPreset maps a preset name → control.SetPreset (re-plays if ON), then
// requests a state publish.
func (s *Service) onPreset(payload []byte) {
	preset := strings.ToLower(strings.TrimSpace(string(payload)))
	if err := s.ctrl.SetPreset(preset); err != nil {
		slog.Warn("mqtt: preset command rejected", "preset", preset, "err", err)
		// State unchanged on rejection; re-publish so HA reverts to the truth.
	}
	s.requestPublish()
}

// onVolume maps a 0–100 integer → control.SetVolume, then requests a state
// publish.
func (s *Service) onVolume(payload []byte) {
	v, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil {
		slog.Warn("mqtt: volume command is not an integer", "payload", string(payload), "err", err)
		s.requestPublish()
		return
	}
	s.ctrl.SetVolume(v)
	s.requestPublish()
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
// (retained), so HA stays in sync after every change. Called from Run.
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
// discovery, mark available, re-assert the authoritative state (NOT stale
// retained values), then ask Run to publish current state. Safe to call on
// every (re)connect. The reassert never stops the speaker: if OFF it leaves it
// alone, and if ON it only re-plays when the target isn't already playing.
//
// State goes through Run rather than being published here so Run is the only
// writer of the state topics: a snapshot taken here could otherwise land after
// (and overwrite) a newer one Run published for a command.
func (s *Service) OnConnect() error {
	if err := s.PublishDiscovery(); err != nil {
		return err
	}
	if err := s.PublishAvailable(); err != nil {
		return err
	}
	s.ctrl.Reassert()
	s.requestPublish()
	return nil
}

func publishOne(b Broker, topic, payload string) {
	if err := b.Publish(topic, []byte(payload), true); err != nil {
		slog.Error("mqtt: publish failed", "topic", topic, "err", err)
	}
}
