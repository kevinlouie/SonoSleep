// Package mqtt publishes Home Assistant MQTT-discovery entities (switch / select /
// number) for the white-noise service, subscribes to their command topics, maps
// commands onto the authoritative control state, and republishes state. See
// .ralph/specs/mqtt-entities.md.
//
// The transport (connect / publish / subscribe / LWT) is abstracted behind the
// Broker interface so the command-mapping, discovery-payload and reconcile logic
// are unit-testable without a live broker.
package mqtt

import "encoding/json"

// Topic constants. The discovery prefix is HA's default "homeassistant"; the
// state/command/availability topics live under the "hwnsonos" namespace.
const (
	discoveryPrefix = "homeassistant"

	// AvailabilityTopic is the LWT topic: "online"/"offline", retained.
	AvailabilityTopic = "hwnsonos/status"
	payloadOnline     = "online"
	payloadOffline    = "offline"

	// Switch (power) topics.
	powerCommandTopic = "hwnsonos/power/set"
	powerStateTopic   = "hwnsonos/power/state"
	payloadOn         = "ON"
	payloadOff        = "OFF"

	// Select (preset) topics.
	presetCommandTopic = "hwnsonos/preset/set"
	presetStateTopic   = "hwnsonos/preset/state"

	// Number (volume) topics.
	volumeCommandTopic = "hwnsonos/volume/set"
	volumeStateTopic   = "hwnsonos/volume/state"

	// Discovery config topics (retained).
	powerDiscoveryTopic  = discoveryPrefix + "/switch/hwn_sonos/power/config"
	presetDiscoveryTopic = discoveryPrefix + "/select/hwn_sonos/preset/config"
	volumeDiscoveryTopic = discoveryPrefix + "/number/hwn_sonos/volume/config"
)

// presetOptions are the select entity's options, in spec order.
var presetOptions = []string{"white", "pink", "brown"}

// device is the shared device block embedded in each entity's discovery payload
// so HA groups all three under one device.
type device struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
}

func sharedDevice() device {
	return device{
		Identifiers:  []string{"hwn_sonos"},
		Name:         "White Noise (Sonos)",
		Manufacturer: "ha-white-noise-sonos",
		Model:        "Go noise service",
	}
}

// switchDiscovery is the discovery payload for the power switch entity.
type switchDiscovery struct {
	Name              string `json:"name"`
	UniqueID          string `json:"unique_id"`
	CommandTopic      string `json:"command_topic"`
	StateTopic        string `json:"state_topic"`
	PayloadOn         string `json:"payload_on"`
	PayloadOff        string `json:"payload_off"`
	Icon              string `json:"icon"`
	AvailabilityTopic string `json:"availability_topic"`
	Device            device `json:"device"`
}

// selectDiscovery is the discovery payload for the preset select entity.
type selectDiscovery struct {
	Name              string   `json:"name"`
	UniqueID          string   `json:"unique_id"`
	CommandTopic      string   `json:"command_topic"`
	StateTopic        string   `json:"state_topic"`
	Options           []string `json:"options"`
	Icon              string   `json:"icon"`
	AvailabilityTopic string   `json:"availability_topic"`
	Device            device   `json:"device"`
}

// numberDiscovery is the discovery payload for the volume number entity.
type numberDiscovery struct {
	Name              string `json:"name"`
	UniqueID          string `json:"unique_id"`
	CommandTopic      string `json:"command_topic"`
	StateTopic        string `json:"state_topic"`
	Min               int    `json:"min"`
	Max               int    `json:"max"`
	Step              int    `json:"step"`
	UnitOfMeasurement string `json:"unit_of_measurement"`
	Icon              string `json:"icon"`
	AvailabilityTopic string `json:"availability_topic"`
	Device            device `json:"device"`
}

// discoveryDoc pairs a retained discovery topic with its JSON payload.
type discoveryDoc struct {
	Topic   string
	Payload []byte
}

// buildSwitchDiscovery returns the power-switch discovery payload (JSON).
func buildSwitchDiscovery() []byte {
	b, _ := json.Marshal(switchDiscovery{
		Name:              "White Noise",
		UniqueID:          "hwn_sonos_power",
		CommandTopic:      powerCommandTopic,
		StateTopic:        powerStateTopic,
		PayloadOn:         payloadOn,
		PayloadOff:        payloadOff,
		Icon:              "mdi:weather-fog",
		AvailabilityTopic: AvailabilityTopic,
		Device:            sharedDevice(),
	})
	return b
}

// buildSelectDiscovery returns the preset-select discovery payload (JSON).
func buildSelectDiscovery() []byte {
	b, _ := json.Marshal(selectDiscovery{
		Name:              "White Noise Preset",
		UniqueID:          "hwn_sonos_preset",
		CommandTopic:      presetCommandTopic,
		StateTopic:        presetStateTopic,
		Options:           presetOptions,
		Icon:              "mdi:tune-variant",
		AvailabilityTopic: AvailabilityTopic,
		Device:            sharedDevice(),
	})
	return b
}

// buildNumberDiscovery returns the volume-number discovery payload (JSON).
func buildNumberDiscovery() []byte {
	b, _ := json.Marshal(numberDiscovery{
		Name:              "White Noise Volume",
		UniqueID:          "hwn_sonos_volume",
		CommandTopic:      volumeCommandTopic,
		StateTopic:        volumeStateTopic,
		Min:               0,
		Max:               100,
		Step:              1,
		UnitOfMeasurement: "%",
		Icon:              "mdi:volume-high",
		AvailabilityTopic: AvailabilityTopic,
		Device:            sharedDevice(),
	})
	return b
}

// discoveryDocs returns all three discovery (topic, payload) pairs.
func discoveryDocs() []discoveryDoc {
	return []discoveryDoc{
		{Topic: powerDiscoveryTopic, Payload: buildSwitchDiscovery()},
		{Topic: presetDiscoveryTopic, Payload: buildSelectDiscovery()},
		{Topic: volumeDiscoveryTopic, Payload: buildNumberDiscovery()},
	}
}
