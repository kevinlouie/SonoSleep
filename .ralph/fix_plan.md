# Ralph Fix Plan

Implement top-down. One item per loop. Specs live in `.ralph/specs/`. Port DSP from
`../reSpeakerSleep`. Do not start Phase N+1 until Phase N's core item works.

## Phase 0 — Scaffolding
- [ ] Init Go module (`go mod init`), create `cmd/hwnsonos` + `internal/{noise,stream,ha,mqtt,config}` skeleton
- [ ] `internal/config`: load env (see `AGENT.md`), `.env.example`, fail fast on missing required vars
- [ ] Health endpoint `GET /healthz` returning 200
- [ ] `Dockerfile` (Go build + ffmpeg) and `docker-compose.yml` (host network, env_file)

## Phase 1 — Noise synthesis (port from reSpeakerSleep)
- [ ] `internal/noise`: white generator (s16le, stereo, 48 kHz) — see `specs/brown-noise-dsp.md`
- [ ] Brown = one-pole lowpass on white, fc ≈ 115 Hz, RMS ≈ 0.3 (no clipping/stutter)
- [ ] Pink = Paul Kellet filter
- [ ] Preset enum {white, pink, brown}; volume scale 0–100 → linear gain
- [ ] Unit tests: RMS within tolerance, brown has more low-freq energy than white (FFT band ratio)

## Phase 2 — Infinite MP3 stream (LOAD-BEARING; de-risk early)
- [ ] `internal/stream`: `GET /stream?preset=brown` → spawn `ffmpeg -f s16le -ar 48000 -ac 2 -i - -f mp3 -`, pipe generated PCM into stdin, copy stdout to response
- [ ] Headers per `specs/sonos-streaming.md`: `Content-Type: audio/mpeg`, chunked, NO `Content-Length`, no caching; flush regularly
- [ ] Clean teardown when client disconnects (kill ffmpeg, stop generator goroutine) — no leaks
- [x] **Sonos compat PRE-VERIFIED (2026-06-19, kitchen + SomaFM Icecast):** plain http URL → UPnP 714; `x-rincon-mp3radio://` scheme → plays; held ~8 min with no drop. Re-run on `media_player.bedroom` once built, but architecture is confirmed.

## Phase 3 — Home Assistant orchestration
- [ ] `internal/ha`: REST client — `play_media`, `volume_set`, `media_stop`, `get_state`
- [ ] Play: `play_media(media_content_id = <PUBLIC_BASE_URL>/stream?preset=<p>, media_content_type = music)` then `volume_set`
- [ ] Handle target `unavailable` (speaker off): retry with backoff, surface state
- [ ] Watchdog: poll/subscribe Sonos state; if idle/paused/recovered while switch ON → re-play (backoff, log gap)

## Phase 4 — MQTT control entities (HA discovery)
- [ ] `internal/mqtt`: connect, publish discovery configs for switch / select / number (see `specs/mqtt-entities.md` + `examples/`)
- [ ] Subscribe command topics; map: switch ON/OFF → play/stop; select → preset (re-play if on); number → volume_set
- [ ] Publish state topics + `availability` (online/offline LWT); reconcile on reconnect
- [ ] Unit-test discovery payload JSON shape

## Phase 5 — Hardening & docs
- [ ] Backoff/jitter on all reconnect loops; structured logging
- [ ] Graceful shutdown (stop stream, media_stop, MQTT offline LWT)
- [ ] README: setup, env, HA token creation, add MQTT integration, troubleshooting (714 MIME, drops)
- [ ] Optional: fade-in on start (ramp gain ~3 s) to match reSpeakerSleep behavior

## Completed
- [x] Project enabled for Ralph
- [x] Research prior art; architecture decided (see `projectplan.md`)
- [x] Ralph files + specs written

## Notes
- The Phase 2 manual Sonos test is the single biggest risk — do it before building
  Phases 3–4 in full. If the stream won't hold, revisit format (FLAC) or add ICY headers.
- Keep `projectplan.md` decisions log updated with anything learned about Sonos behavior.
