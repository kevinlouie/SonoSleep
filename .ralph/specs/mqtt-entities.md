# Spec: MQTT discovery entities

The Go service publishes Home Assistant MQTT discovery configs so HA auto-creates the
control entities. HA already has the `mqtt` integration + a broker on the Synology host.

Discovery prefix: `homeassistant` (HA default). Device-grouped so all three entities show
under one device "White Noise (Sonos)".

Shared device block (embed in each entity's discovery payload):
```json
{
  "device": {
    "identifiers": ["hwn_sonos"],
    "name": "White Noise (Sonos)",
    "manufacturer": "ha-white-noise-sonos",
    "model": "Go noise service"
  },
  "availability_topic": "hwnsonos/status"
}
```
LWT: publish `online`/`offline` to `hwnsonos/status` (retained), set as MQTT will.

## Entities

### Switch — on/off
- discovery: `homeassistant/switch/hwn_sonos/power/config`
- `command_topic`: `hwnsonos/power/set`  (payload `ON`/`OFF`)
- `state_topic`:   `hwnsonos/power/state`
- ON → HA play_media(current preset) + volume_set; OFF → media_stop.

### Select — preset
- discovery: `homeassistant/select/hwn_sonos/preset/config`
- `options`: `["white","pink","brown"]`
- `command_topic`: `hwnsonos/preset/set`
- `state_topic`:   `hwnsonos/preset/state`
- On change while ON → re-play with new preset URL.

### Number — volume (0–100)
- discovery: `homeassistant/number/hwn_sonos/volume/config`
- `min`: 0, `max`: 100, `step`: 1, `unit_of_measurement`: "%"
- `command_topic`: `hwnsonos/volume/set`
- `state_topic`:   `hwnsonos/volume/state`
- On change → HA volume_set(value/100) on the Sonos.

## Behavior
- Publish all discovery configs (retained) on startup/MQTT-reconnect.
- Publish current state to each `state_topic` after every change (so HA stays in sync).
- Reconcile on reconnect: re-assert switch/preset/volume from the service's authoritative
  state, not from stale retained values.
- See `examples/mqtt-discovery-switch.json` for a full payload sample.
