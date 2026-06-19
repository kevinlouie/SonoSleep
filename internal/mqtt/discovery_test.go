package mqtt

import (
	"encoding/json"
	"testing"
)

// decode unmarshals a discovery payload into a generic map for shape assertions.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, b)
	}
	return m
}

// assertDevice checks the shared device block is correct and device-grouped.
func assertDevice(t *testing.T, m map[string]any) {
	t.Helper()
	dev, ok := m["device"].(map[string]any)
	if !ok {
		t.Fatalf("missing device block")
	}
	ids, ok := dev["identifiers"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "hwn_sonos" {
		t.Errorf("device.identifiers = %v, want [hwn_sonos]", dev["identifiers"])
	}
	if dev["name"] != "White Noise (Sonos)" {
		t.Errorf("device.name = %v", dev["name"])
	}
	if dev["manufacturer"] != "ha-white-noise-sonos" {
		t.Errorf("device.manufacturer = %v", dev["manufacturer"])
	}
	if dev["model"] != "Go noise service" {
		t.Errorf("device.model = %v", dev["model"])
	}
	if m["availability_topic"] != AvailabilityTopic {
		t.Errorf("availability_topic = %v, want %s", m["availability_topic"], AvailabilityTopic)
	}
}

func TestSwitchDiscoveryShape(t *testing.T) {
	m := decode(t, buildSwitchDiscovery())
	assertDevice(t, m)
	checks := map[string]any{
		"unique_id":     "hwn_sonos_power",
		"command_topic": "hwnsonos/power/set",
		"state_topic":   "hwnsonos/power/state",
		"payload_on":    "ON",
		"payload_off":   "OFF",
	}
	for k, want := range checks {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
}

func TestSelectDiscoveryShape(t *testing.T) {
	m := decode(t, buildSelectDiscovery())
	assertDevice(t, m)
	if m["unique_id"] != "hwn_sonos_preset" {
		t.Errorf("unique_id = %v", m["unique_id"])
	}
	if m["command_topic"] != "hwnsonos/preset/set" {
		t.Errorf("command_topic = %v", m["command_topic"])
	}
	if m["state_topic"] != "hwnsonos/preset/state" {
		t.Errorf("state_topic = %v", m["state_topic"])
	}
	opts, ok := m["options"].([]any)
	if !ok || len(opts) != 3 {
		t.Fatalf("options = %v, want 3 entries", m["options"])
	}
	want := []string{"white", "pink", "brown"}
	for i, w := range want {
		if opts[i] != w {
			t.Errorf("options[%d] = %v, want %s", i, opts[i], w)
		}
	}
}

func TestNumberDiscoveryShape(t *testing.T) {
	m := decode(t, buildNumberDiscovery())
	assertDevice(t, m)
	if m["unique_id"] != "hwn_sonos_volume" {
		t.Errorf("unique_id = %v", m["unique_id"])
	}
	if m["command_topic"] != "hwnsonos/volume/set" {
		t.Errorf("command_topic = %v", m["command_topic"])
	}
	if m["state_topic"] != "hwnsonos/volume/state" {
		t.Errorf("state_topic = %v", m["state_topic"])
	}
	// JSON numbers decode to float64.
	for k, want := range map[string]float64{"min": 0, "max": 100, "step": 1} {
		if v, _ := m[k].(float64); v != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if m["unit_of_measurement"] != "%" {
		t.Errorf("unit_of_measurement = %v, want %%", m["unit_of_measurement"])
	}
}

func TestDiscoveryTopics(t *testing.T) {
	docs := discoveryDocs()
	wantTopics := []string{
		"homeassistant/switch/hwn_sonos/power/config",
		"homeassistant/select/hwn_sonos/preset/config",
		"homeassistant/number/hwn_sonos/volume/config",
	}
	if len(docs) != len(wantTopics) {
		t.Fatalf("got %d discovery docs, want %d", len(docs), len(wantTopics))
	}
	for i, w := range wantTopics {
		if docs[i].Topic != w {
			t.Errorf("docs[%d].Topic = %s, want %s", i, docs[i].Topic, w)
		}
		if len(docs[i].Payload) == 0 {
			t.Errorf("docs[%d] has empty payload", i)
		}
	}
}
