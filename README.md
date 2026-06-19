# ha-white-noise-sonos

A small **Go service** that synthesizes continuous brown / pink / white noise (no audio
files, no loop seam) and plays it to a **Sonos** speaker through Home Assistant.

It is the successor to [`../reSpeakerSleep`](../reSpeakerSleep) — same noise, but instead
of generating it on an ESP32 and playing over a 3.5 mm jack, it generates server-side and
streams straight to the Sonos over the LAN. The proven DSP (one-pole-lowpass brown, Paul
Kellet pink) ports over directly.

## How it works
- Serves an **infinite chunked MP3 stream** (`GET /stream?preset=brown`) — Icecast-radio
  style, so Sonos accepts and holds it.
- Publishes **Home Assistant control entities** (switch / preset select / volume number)
  via MQTT discovery.
- Orchestrates playback by calling the **HA REST API** (`play_media`, `volume_set`,
  `media_stop`) on `media_player.bedroom`.
- A **watchdog** re-arms playback if Sonos drops the long stream.

```
Go service ──MP3 stream──> Sonos (media_player.bedroom)
     │  └──MQTT discovery──> HA entities (switch/select/number)
     └──REST API──────────> HA ──> Sonos (play / volume / stop)
```

## Status
Early scaffolding. Design lives in [`projectplan.md`](projectplan.md); task breakdown and
specs are under [`.ralph/`](.ralph/) (this repo is developed with the Ralph autonomous
loop). The **load-bearing unknown** is whether the bedroom Sonos holds the stream for
hours — verified manually first (see `.ralph/specs/sonos-streaming.md`).

## Quick start (once built)
```bash
cp .env.example .env   # fill in HA token, host IPs
docker compose -f .ralph/examples/docker-compose.yml up --build -d
```
Add the **MQTT** integration in Home Assistant; the "White Noise (Sonos)" device appears
automatically. See `.ralph/AGENT.md` for build/run details.
