# ha-white-noise-sonos — Project Plan

## Goal
A standalone **Go service** that synthesizes continuous brown / pink / white noise live
(no audio files, no loop seam) and plays it to a **Sonos** speaker
(`media_player.bedroom`) through Home Assistant. The service owns its Home Assistant
control entities (switch / preset / volume) via MQTT discovery and orchestrates
playback by calling the HA REST API. It also re-arms playback when Sonos drops the
infinite stream.

This is the successor to **`../reSpeakerSleep`**, which synthesized the same noise on a
ReSpeaker Lite (ESP32-S3) and played it over a 3.5 mm jack. That device is now wired to
a Sonos, so the on-device microcontroller is redundant: we can generate the noise
server-side and stream it straight to the Sonos over the LAN. The proven DSP
(one-pole-lowpass brown, Paul Kellet pink) ports over directly.

## Prior art (researched 2026-06-19)
- **`nirnachmani/noise-generator`** — HA custom integration (Python, "vibe coded") that
  generates white/pink/brown/custom noise as infinite **Media Source** streams playable
  via `media_player.play_media`. Targets Google Home; **no documented Sonos support**, no
  control over HTTP stream headers. Rejected in favor of a Go service per project intent
  (decoupled from HA, portable, full control over the stream — which matters for Sonos).
- **Music Assistant** — not installed on this HA. It would handle radio reconnect for
  us; absent it, the Go watchdog covers that role.

## Key decisions

| Decision | Choice | Rationale |
|---|---|---|
| Language / form | **Standalone Go HTTP service** | User intent; decouples noise gen from HA; full control of stream headers (needed for Sonos). |
| Target speaker | **`media_player.bedroom`** (Sonos) | Bedside white-noise speaker. Currently `unavailable` (powered down) — must handle that gracefully. |
| Noise DSP | Port from reSpeakerSleep: **brown** = one-pole lowpass on white (fc ≈ 115 Hz, RMS ≈ 0.3); **pink** = Paul Kellet; **white** = raw | Proven to sound right and not stutter/clip. No need to re-derive. |
| Stream format | **MP3** (CBR), `Content-Type: audio/mpeg`, **chunked / no `Content-Length`** | Icecast-radio semantics. Avoids Sonos UPnP 714 "Illegal MIME-Type" and the finite-track timeout. PCM→MP3 via piped **ffmpeg**. |
| HA control surface | **Full entities** via **MQTT discovery**: switch (on/off), select (white/pink/brown), number (volume 0–100) | Replicates the reSpeakerSleep UX. HA already has the `mqtt` domain + broker. Go publishes discovery, subscribes to command topics. |
| Playback orchestration | Go calls **HA REST API** (long-lived token): `play_media`, `volume_set`, `media_stop` on the bedroom Sonos | Keeps all logic in Go. HA only needs MQTT + a token. |
| Infinite-stream drop | **Watchdog**: Go watches the Sonos `media_player` state; if it goes `idle`/`paused` while the switch is ON, re-issue `play_media` (≈1–2 s gap) | Sonos can drop long HTTP streams. This is the reconnect strategy (Music Assistant would otherwise do it). |
| Deploy | **Docker on the Synology HA host** | Same LAN as the Sonos and HA; mirrors the reSpeakerSleep/Wyoming stack pattern. |

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
        ├─ HTTP stream  : GET /stream?preset=brown  → infinite chunked MP3
        │     noise_gen (PCM s16le) ─pipe─> ffmpeg (-f mp3 -) ─> chunked body
        └─ noise_gen    : white / pink / brown synthesis
                                                  │
   Sonos ── fetches ──> http://<host>:<port>/stream?preset=brown  (LAN)
```

### Playback flow
1. User flips the **White Noise** switch ON in HA → MQTT command → Go.
2. Go calls HA `media_player.play_media` on `media_player.bedroom` with
   `media_content_id = http://<host>:<port>/stream?preset=<current>`,
   `media_content_type = music`, then `media_player.volume_set` to the number entity.
3. Sonos opens the stream; Go's HTTP handler starts a per-connection noise→ffmpeg→MP3
   pipeline and streams it indefinitely.
4. Watchdog: if the Sonos reports `idle`/`paused`/`unavailable→available` while the
   switch is ON, Go re-issues `play_media`.
5. Switch OFF → Go calls `media_player.media_stop`; the stream connection closes.

## Open questions / risks
- **Infinite-stream longevity (load-bearing):** must verify the bedroom Sonos plays a
  chunked MP3 stream for hours, and measure if/when it drops, to tune the watchdog. This
  is **task 1** — test against the real device early. Sonos is currently `unavailable`,
  so this needs the speaker powered on (and low volume — it's a bedroom).
- **714 MIME / content-type:** confirm `media_content_type: music` + `audio/mpeg` is
  accepted by this Sonos; some setups need a `.mp3`-suffixed URL.
- **ffmpeg dependency:** ship it in the container. Alternative: cgo libmp3lame. ffmpeg
  pipe chosen for simplicity and matches the existing homelab tooling.
- **Multiple Sonos consumers / re-buffering gap:** watchdog re-play causes a ~1–2 s gap;
  acceptable for sleep. Avoid tight reconnect loops (backoff).
- **HA token storage:** long-lived token in env/secret, not committed.

## Status / decisions log
- 2026-06-19: Kickoff. Researched prior art (`nirnachmani/noise-generator`, Music
  Assistant). Confirmed via live HA MCP that `media_player.bedroom`, `living_room`,
  `kitchen` are Sonos (`x-sonos-htastream`/RINCON); `sonos` integration present, no
  Music Assistant. Decided: Go service + MP3 chunked stream + MQTT entities + HA REST
  orchestration + watchdog reconnect. Target = bedroom. Deploy = Synology docker.
- 2026-06-19: **Live Sonos test (kitchen, SomaFM Icecast MP3).** Plain `http://` URL +
  `media_content_type: music` → **UPnP 714 "Illegal MIME-Type"** (reproduced). Fix:
  prefix with **`x-rincon-mp3radio://`** scheme → plays. Go HA client must emit
  `x-rincon-mp3radio://http://<host>:<port>/stream?preset=<p>`. See memory
  `sonos-714-mp3radio-scheme`. Duration-hold test (>5 min) in progress.
- 2026-06-19: **Duration test PASSED.** Kitchen held the Icecast MP3 stream ~8 min
  continuous, no drop/idle/pause; SomaFM track titles rotated normally. The ~300 s Sonos
  cutoff did NOT occur. Architecture validated: `x-rincon-mp3radio://` + infinite chunked
  MP3 plays indefinitely. Watchdog reconnect stays in scope as a safety net but is not the
  primary mechanism. Kitchen restored (stopped, volume 0.2).
