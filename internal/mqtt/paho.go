package mqtt

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
)

// publishTimeout bounds how long a single publish waits for the broker token.
const publishTimeout = 10 * time.Second

// pahoBroker adapts paho.mqtt.golang to the Broker interface.
type pahoBroker struct {
	client pahomqtt.Client
}

// Publish implements Broker.
func (b *pahoBroker) Publish(topic string, payload []byte, retained bool) error {
	tok := b.client.Publish(topic, 1, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("mqtt: publish %s timed out", topic)
	}
	return tok.Error()
}

// Subscribe implements Broker. paho delivers messages on its own goroutines.
func (b *pahoBroker) Subscribe(topic string, handler func(payload []byte)) error {
	tok := b.client.Subscribe(topic, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
		handler(m.Payload())
	})
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("mqtt: subscribe %s timed out", topic)
	}
	return tok.Error()
}

// Connect dials the broker, builds the Service, and wires the on-connect handler
// so discovery/availability/state are (re)published and the control state is
// re-asserted on every connect AND reconnect. The LWT is set to publish "offline"
// (retained) to the availability topic if the connection drops ungracefully.
//
// ctx is used for the control reassert calls inside OnConnect (cancel it on
// shutdown so a reconcile in flight unwinds). It returns the live Service and a
// disconnect func for graceful shutdown (publish offline, then disconnect).
func Connect(ctx context.Context, broker, user, pass string, ctrl Controller) (*Service, func(), error) {
	// Reconnect policy: paho's auto-reconnect uses capped exponential backoff
	// between MaxReconnectInterval bounds; the initial connect retry is jittered
	// (5s ± up to 5s) so a fleet of clients doesn't reconnect in lockstep after a
	// broker restart.
	retryInterval := time.Duration(float64(5*time.Second) * (1.0 + rand.Float64()))
	opts := pahomqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID("hwnsonos").
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(2*time.Minute).
		SetConnectRetry(true).
		SetConnectRetryInterval(retryInterval).
		SetWill(AvailabilityTopic, payloadOffline, 1, true)
	if user != "" {
		opts.SetUsername(user)
	}
	if pass != "" {
		opts.SetPassword(pass)
	}

	pb := &pahoBroker{}
	svc := NewService(pb, ctrl)

	// On every (re)connect: re-subscribe and run the reconcile sequence.
	opts.SetOnConnectHandler(func(_ pahomqtt.Client) {
		if err := svc.Subscribe(ctx); err != nil {
			slog.Error("mqtt: subscribe on connect failed", "err", err)
		}
		if err := svc.OnConnect(ctx); err != nil {
			slog.Error("mqtt: on-connect publish failed", "err", err)
		}
		slog.Info("mqtt: connected (discovery + state published)", "broker", broker)
	})
	opts.SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
		slog.Warn("mqtt: connection lost, will reconnect with backoff", "err", err)
	})

	client := pahomqtt.NewClient(opts)
	pb.client = client

	tok := client.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return nil, nil, fmt.Errorf("mqtt: connect to %s timed out", broker)
	}
	if err := tok.Error(); err != nil {
		return nil, nil, fmt.Errorf("mqtt: connect: %w", err)
	}

	disconnect := func() {
		// Publish offline explicitly (retained) before a clean disconnect; the
		// LWT only fires on ungraceful drops.
		if err := svc.PublishOffline(); err != nil {
			slog.Warn("mqtt: publish offline on shutdown failed", "err", err)
		}
		client.Disconnect(250)
	}
	return svc, disconnect, nil
}
