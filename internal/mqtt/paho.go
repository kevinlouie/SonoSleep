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

// offlineTimeout bounds the "offline" publish on shutdown, so a dead broker
// can't eat the container's stop grace period.
const offlineTimeout = time.Second

// writeTimeout bounds how long paho blocks handing a publish to its network
// writer. paho's default (0) waits up to 30 s inside Publish itself, before any
// token exists to time out on.
const writeTimeout = 2 * time.Second

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

// Subscribe implements Broker. paho's ordered router calls handler synchronously,
// so handler must not block (Service's handlers only record intent).
func (b *pahoBroker) Subscribe(topic string, handler func(payload []byte)) error {
	tok := b.client.Subscribe(topic, 1, func(_ pahomqtt.Client, m pahomqtt.Message) {
		handler(m.Payload())
	})
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("mqtt: subscribe %s timed out", topic)
	}
	return tok.Error()
}

// clientOptions builds the paho options: reconnect policy, LWT, write timeout
// and credentials. Split out of Connect so it is unit-testable.
func clientOptions(broker, user, pass string) *pahomqtt.ClientOptions {
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
		SetWriteTimeout(writeTimeout).
		SetWill(AvailabilityTopic, payloadOffline, 1, true)
	if user != "" {
		opts.SetUsername(user)
	}
	if pass != "" {
		opts.SetPassword(pass)
	}
	return opts
}

// Connect dials the broker, builds the Service, and wires the on-connect handler
// so discovery/availability/state are (re)published and the control state is
// re-asserted on every connect AND reconnect. The LWT is set to publish "offline"
// (retained) to the availability topic if the connection drops ungracefully.
//
// ctx bounds the Service's state publisher goroutine (Run). It returns the live
// Service and a disconnect func for graceful shutdown (publish offline, then
// disconnect).
func Connect(ctx context.Context, broker, user, pass string, ctrl Controller) (*Service, func(), error) {
	opts := clientOptions(broker, user, pass)

	pb := &pahoBroker{}
	svc := NewService(pb, ctrl)
	go svc.Run(ctx)

	// On every (re)connect: re-subscribe and run the reconcile sequence. paho
	// runs this handler on its own goroutine, so the publish waits are fine here.
	opts.SetOnConnectHandler(func(_ pahomqtt.Client) {
		if err := svc.Subscribe(); err != nil {
			slog.Error("mqtt: subscribe on connect failed", "err", err)
		}
		if err := svc.OnConnect(); err != nil {
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
		// LWT only fires on ungraceful drops. Short wait: shutdown is time-boxed.
		tok := client.Publish(AvailabilityTopic, 1, true, []byte(payloadOffline))
		if !tok.WaitTimeout(offlineTimeout) {
			slog.Warn("mqtt: publish offline on shutdown timed out")
		} else if err := tok.Error(); err != nil {
			slog.Warn("mqtt: publish offline on shutdown failed", "err", err)
		}
		client.Disconnect(250)
	}
	return svc, disconnect, nil
}
