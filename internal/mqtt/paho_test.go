package mqtt

import "testing"

func TestClientOptions(t *testing.T) {
	opts := clientOptions("tcp://broker:1883", "user", "pass")
	// paho's default WriteTimeout (0) blocks Publish up to 30 s before any
	// token exists, which would blow the shutdown budget on a dead broker.
	if opts.WriteTimeout != writeTimeout || writeTimeout <= 0 {
		t.Errorf("WriteTimeout = %v, want %v", opts.WriteTimeout, writeTimeout)
	}
	if opts.WillTopic != AvailabilityTopic || string(opts.WillPayload) != payloadOffline || !opts.WillRetained {
		t.Errorf("LWT = %q %q retained=%v, want %q offline retained", opts.WillTopic, opts.WillPayload, opts.WillRetained, AvailabilityTopic)
	}
	if opts.Username != "user" || opts.Password != "pass" {
		t.Errorf("credentials = %q/%q", opts.Username, opts.Password)
	}
}
