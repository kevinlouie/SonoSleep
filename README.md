# ha-white-noise-sonos

A small **Go service** that synthesizes continuous brown / pink / white noise (no audio
files, no loop seam) and plays it to a **Sonos** speaker through Home Assistant.

It is the successor to [reSpeakerSleep](https://github.com/kevinlouie/reSpeakerSleep) — same noise, but instead
of generating it on an ESP32 and playing over a 3.5 mm jack, it generates server-side and
streams straight to the Sonos over the LAN. The proven DSP (one-pole-lowpass brown, Paul
Kellet pink) ports over directly.

## What it does

- Serves an **infinite chunked MP3 stream** (`GET /stream?preset=brown`) — Icecast-radio
  style, so the Sonos accepts and holds it for hours.
- Publishes **Home Assistant control entities** (a switch, a preset select, and a volume
  number) via MQTT discovery, grouped under one device **"White Noise (Sonos)"**.
- Orchestrates playback by calling the **HA REST API** (`play_media`, `volume_set`,
  `media_stop`) on the target `media_player` (default `media_player.bedroom`).
- Runs a **watchdog** that re-arms playback if the Sonos drops the long stream.
- Eases playback in with a ~3 s **fade-in** gain ramp at stream start.

## Architecture

```
Synology docker host
 ├─ Home Assistant ── sonos integration ──> Sonos (media_player.bedroom)
 │     ▲  REST API (LLAT)         ▲ play_media(url) / volume_set / media_stop
 │     │                          │
 │  MQTT broker ──discovery──> switch / select / number entities
 │     ▲ command topics
 │     │
 └─ ha-white-noise-sonos (Go) ─────────────────┘
        ├─ MQTT client  : publish discovery, subscribe commands, publish state
        ├─ Orchestrator : on switch/preset/volume → call HA REST; watchdog re-play
        ├─ HTTP stream  : GET /stream?preset=brown → infinite chunked MP3
        │     noise_gen (PCM s16le) ─pipe─> ffmpeg (-f mp3 -) ─> chunked body
        └─ noise_gen    : white / pink / brown synthesis
                                                  │
   Sonos ── fetches ──> http://<host>:8099/stream?preset=brown  (LAN)
```

### Playback flow

1. User flips the **White Noise** switch ON in HA → MQTT command → Go service.
2. Go calls HA `media_player.play_media` on the target with
   `media_content_id = x-rincon-mp3radio://<PUBLIC_BASE_URL>/stream?preset=<current>`,
   `media_content_type = music`, then `media_player.volume_set` to the number entity's value.
3. The Sonos opens the stream; the HTTP handler starts a per-connection
   noise → ffmpeg → MP3 pipeline and streams it indefinitely.
4. Watchdog: if the Sonos reports `idle`/`paused`, or recovers from `unavailable`, while
   the switch is ON, Go re-issues `play_media` (with a suppress window after a deliberate
   stop so it never fights an intended pause/announcement).
5. Switch OFF → Go calls `media_player.media_stop`; the stream connection closes.

## Requirements

- **Docker** on a host on the same LAN as the Sonos, HA, and MQTT broker (the Synology HA
  host). `ffmpeg` is bundled in the image; for a local (non-Docker) run you need `ffmpeg`
  on `PATH` and Go ≥ 1.24.
- A reachable **MQTT broker** (Home Assistant's broker) and the HA **MQTT integration**
  enabled.
- A Home Assistant **long-lived access token** for REST orchestration.

## Setup

### 1. Configure environment

Copy the example and fill it in. `.env` is gitignored — never commit a real token.

```bash
cp .env.example .env
```

| Variable | Required | Description |
|---|:---:|---|
| `HWN_HTTP_ADDR` | | Stream + health listen address. Default `:8099`. |
| `HWN_PUBLIC_BASE_URL` | ✓ | URL the **Sonos** fetches the stream from. MUST be LAN-reachable — use the Synology host IP/hostname, **not** `localhost`. e.g. `http://192.168.1.50:8099`. |
| `HWN_HA_BASE_URL` | ✓ | HA REST base, e.g. `http://homeassistant.local:8123`. |
| `HWN_HA_TOKEN` | ✓ | HA long-lived access token (see below). |
| `HWN_HA_MEDIA_PLAYER` | ✓ | Target entity, e.g. `media_player.bedroom`. |
| `HWN_MQTT_BROKER` | ✓ | Broker URL, e.g. `tcp://192.168.1.50:1883`. |
| `HWN_MQTT_USER` / `HWN_MQTT_PASS` | | Broker credentials (optional). |
| `HWN_DEFAULT_PRESET` | | `white` \| `pink` \| `brown`. Default `brown`. |
| `HWN_DEFAULT_VOLUME` | | `0`–`100`. Default `80`. |
| `HWN_LOG_LEVEL` | | `debug` \| `info` \| `warn` \| `error`. Default `info`. Structured (`log/slog`) to stderr. |

The full list also lives in [`.env.example`](.env.example).

### 2. Create a Home Assistant long-lived access token

1. In Home Assistant, click your **profile** (bottom-left, your user name).
2. Scroll to **Long-lived access tokens** → **Create Token**.
3. Name it `hwnsonos`, copy the token **once** (HA shows it only at creation), and paste it
   into `.env` as `HWN_HA_TOKEN`.

### 3. Add the MQTT integration in Home Assistant

The control entities arrive via MQTT discovery, so HA needs the MQTT integration pointed at
the same broker the service uses (`HWN_MQTT_BROKER`).

1. **Settings → Devices & Services → Add Integration → MQTT.**
2. Enter the broker host/port and credentials (the same broker the service connects to).
3. Leave **Enable discovery** on (default); the discovery prefix is `homeassistant`.

If you already use Home Assistant's built-in **Mosquitto broker** add-on, the MQTT
integration is typically already configured — just make sure `HWN_MQTT_BROKER` points at it.

### 4. Build and run

**Docker (recommended — Synology host):**

```bash
docker compose up --build -d        # uses the root docker-compose.yml + .env
docker compose logs -f hwnsonos
```

The compose file runs with `network_mode: host` so the Sonos can reach the stream port and
the service can reach the broker/HA on the LAN, and a healthcheck hits `/healthz`.

**Local (for development / smoke tests):**

```bash
go build ./...
go test ./...
go run ./cmd/hwnsonos                # requires ffmpeg + a filled-in .env (exported)

# smoke-test the stream without HA/Sonos (writes ~10s of MP3, then probes it):
curl -s "http://localhost:8099/stream?preset=brown" --max-time 10 -o /tmp/brown.mp3 \
  && ffprobe /tmp/brown.mp3
```

### 5. The three entities in Home Assistant

Once the service connects to MQTT, a device **White Noise (Sonos)** appears under
**Settings → Devices & Services → MQTT** with three entities:

| Entity | Type | Effect |
|---|---|---|
| **White Noise** | switch | ON → play the stream + set volume; OFF → `media_stop`. |
| **White Noise Preset** | select (`white`/`pink`/`brown`) | Changes the noise colour; re-plays with the new preset URL if currently ON. |
| **White Noise Volume** | number (0–100 %) | Sets the **Sonos** volume via `volume_set`. |

State/command topics live under the `hwnsonos/` namespace (e.g. `hwnsonos/power/set`,
`hwnsonos/preset/state`, `hwnsonos/volume/state`), and availability is published to
`hwnsonos/status` (`online`/`offline`, with an MQTT LWT for ungraceful disconnects).

## Deploy on Synology (docker-compose)

1. Copy this repo to the Synology host (same LAN as the Sonos / HA / broker).
2. Create `.env` next to `docker-compose.yml` (step 1 above), with `HWN_PUBLIC_BASE_URL`
   set to the **Synology host IP** and port (e.g. `http://192.168.1.50:8099`).
3. From the repo directory:
   ```bash
   docker compose up --build -d
   ```
   `restart: unless-stopped` brings it back after reboots; the `/healthz` healthcheck lets
   Docker/Synology report container health.
4. Flip the **White Noise** switch in HA. If the Sonos is powered off it reports
   `unavailable`; the service backs off and re-plays automatically once it comes back.

The service shuts down gracefully on `SIGINT`/`SIGTERM` (Docker stop): it drains the HTTP
server, publishes MQTT `offline`, issues `media_stop` if it was playing, and disconnects
from the broker — all within a bounded timeout.

## Troubleshooting

### Sonos error UPnP 714 "Illegal MIME-Type" / nothing plays

A plain `http://` `media_content_id` is rejected by Sonos with **UPnP 714** even for a
valid MP3 stream. The fix (already built into this service) is the Sonos
**`x-rincon-mp3radio://`** URI scheme, which makes the speaker treat the URL as an MP3 radio
stream and skip the MIME check. The service emits:

```
x-rincon-mp3radio://http://<HWN_PUBLIC_BASE_URL host>:8099/stream?preset=<preset>
```

If you still see 714: confirm `HWN_PUBLIC_BASE_URL` is a **LAN IP/hostname the Sonos can
reach** (not `localhost`/`127.0.0.1`), and that `/stream` returns `Content-Type: audio/mpeg`
with **no** `Content-Length` (chunked).

### Stream drops after a while / playback stops on its own

Sonos can drop very long HTTP streams. The **watchdog** polls the `media_player` state
(every ~10 s) and re-issues `play_media` when it sees `idle`/`paused` while the switch is
ON. A brief ~1–2 s re-buffer gap is expected and acceptable. Look for
`watchdog: target stopped while ON, re-issuing play` in the logs. If it re-plays too
aggressively during a deliberate pause or a TTS/alarm announcement, note that a 15 s
suppress window already follows any deliberate stop.

### Speaker shows `unavailable`

The Sonos is powered off or off the network. `play_media` probes the state first and backs
off with **capped exponential backoff + jitter** rather than spinning, logging
`ha: target unavailable, backing off`. Power the speaker on; the watchdog detects the
recovery edge and re-plays automatically. (The bedroom Sonos is often powered down between
sleeps — this is expected.)

### `ffmpeg` missing / `encoder start failed`

The `/stream` endpoint pipes PCM through `ffmpeg`. The Docker image bundles it; for a local
run install it (`ffmpeg` on `PATH`). A 500 `encoder start failed` or `ffmpeg ...` warnings
in the logs mean ffmpeg isn't found or errored — verify with `ffmpeg -version`.

### MQTT not connecting / entities don't appear

- Check `HWN_MQTT_BROKER` is correct and reachable from the service host
  (`tcp://host:1883`), and credentials match.
- The service auto-reconnects with capped exponential backoff; watch for
  `mqtt: connection lost, will reconnect with backoff` and `mqtt: connected`.
- If the device never shows up in HA, confirm the **MQTT integration** is added in HA and
  pointed at the **same broker**, and that discovery is enabled (prefix `homeassistant`).
- On (re)connect the service re-publishes discovery + re-asserts state, so a restarted
  broker recovers without intervention.

## Project layout

| Path | Responsibility |
|---|---|
| `cmd/hwnsonos` | main: config load, slog setup, HTTP server, MQTT, watchdog, graceful shutdown. |
| `internal/config` | env loading + validation (fail-fast on missing required vars). |
| `internal/logging` | `log/slog` setup + log-level parsing. |
| `internal/noise` | white/pink/brown PCM synthesis (s16le, stereo, 48 kHz) + fade-in. |
| `internal/stream` | `/stream` handler: PCM → ffmpeg → chunked MP3 per connection. |
| `internal/ha` | HA REST client (`play_media`/`volume_set`/`media_stop`/`get_state`) + watchdog. |
| `internal/mqtt` | MQTT-discovery entities + command/state handling. |
| `internal/control` | authoritative `{on, preset, volume}` state (single source of truth). |

Design rationale lives in [`projectplan.md`](projectplan.md); task breakdown and specs are
under [`.ralph/`](.ralph/) (this repo is developed with the Ralph autonomous loop).
Automated tests cover DSP, backoff, log-level parsing, discovery payloads, and command
mapping; live Sonos playback is verified manually against `media_player.bedroom`.
