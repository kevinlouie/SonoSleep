# Ralph Fix Plan

Implement top-down. One item per loop. Specs live in `.ralph/specs/`. Port DSP from
`../reSpeakerSleep`. Do not start Phase N+1 until Phase N's core item works.

## Phase 0 — Scaffolding
- [x] Init Go module (`go mod init`), create `cmd/hwnsonos` + `internal/config` skeleton (other `internal/*` pkgs created as their phases land)
- [x] `internal/config`: load env (see `AGENT.md`), fail fast on missing required vars (+ unit tests). `.env.example` already present
- [x] Health endpoint `GET /healthz` returning 200 (in `cmd/hwnsonos/main.go`)
- [x] `Dockerfile` (Go build + ffmpeg runtime) and root `docker-compose.yml` (host network, env_file)

## Phase 1 — Noise synthesis (port from reSpeakerSleep)
- [x] `internal/noise`: white generator (s16le, stereo, 48 kHz) — see `specs/brown-noise-dsp.md`
- [x] Brown = one-pole lowpass on white, fc ≈ 115 Hz, RMS ≈ 0.3 (no clipping/stutter)
- [x] Pink = Paul Kellet filter
- [x] Preset enum {white, pink, brown}; volume scale 0–100 → linear gain (`VolumeGain`, HA-side)
- [x] Unit tests: RMS within tolerance, brown has more low-freq energy than white (FFT band ratio). **PASS (verified by run):** white RMS 0.250, brown 0.310, pink 0.201; low-band(<500Hz) fraction white 0.022 / pink 0.619 / brown 0.862. `go vet`+`gofmt` clean.

## Phase 2 — Infinite MP3 stream (LOAD-BEARING; de-risk early)
- [x] `internal/stream`: `GET /stream?preset=brown` → spawn `ffmpeg -f s16le -ar 48000 -ac 2 -i - -c:a libmp3lame -b:a 192k -f mp3 -`, pipe generated PCM into stdin, copy stdout to response. Wired into `main.go` (`mux.Handle("/stream", ...)`). **VERIFIED BY RUN:** real-ffmpeg test produces valid MP3 (frame sync 0xFF Ex).
- [x] Headers per `specs/sonos-streaming.md`: `Content-Type: audio/mpeg`, chunked (no `Content-Length` → resp.ContentLength == -1), `Cache-Control: no-cache, no-store`, flush after every block + after headers. Bad preset → 400.
- [x] Clean teardown when client disconnects (`exec.CommandContext` SIGKILLs ffmpeg → stdin pipe closes → feeder goroutine exits; deferred `cmd.Wait` reaps). **VERIFIED BY RUN under `-race`:** teardown test cancels mid-stream, asserts process reaped (onStreamEnd hook).
- [x] **Sonos compat PRE-VERIFIED (2026-06-19, kitchen + SomaFM Icecast):** plain http URL → UPnP 714; `x-rincon-mp3radio://` scheme → plays; held ~8 min with no drop. Re-run on `media_player.bedroom` once built, but architecture is confirmed.

## Phase 3 — Home Assistant orchestration
- [x] `internal/ha`: REST client — `play_media`, `volume_set`, `media_stop`, `get_state`. **VERIFIED BY RUN:** 19 tests pass (incl. under `-race`); `go build/vet` + `gofmt -l internal/ha` clean. Coverage: play_media request shape (POST `/api/services/media_player/play_media`, `Bearer` auth, `application/json`, body asserts the `x-rincon-mp3radio://http://host:8099/stream?preset=brown` content_id + `media_content_type:music`), volume_set (0–100→0.0–1.0 incl. clamp), media_stop body, get_state path/auth/decode, backoff-on-unavailable (probe→sleep→probe until available, then play issued) + ErrUnavailable after maxRetries, callService error-status wrapping, and watchdog decision logic (idle/paused→replay; off/suppressed/playing/still-unavailable→no replay; unavailable→recovery edge→replay; get_state error→no replay; Run cancels on ctx). Sleeps/jitter/clock injected so tests run in ~1s, no live network.
- [x] Play: `play_media(media_content_id = <PUBLIC_BASE_URL>/stream?preset=<p>, media_content_type = music)` then `volume_set`
- [x] Handle target `unavailable` (speaker off): retry with backoff, surface state
- [x] Watchdog: poll/subscribe Sonos state; if idle/paused/recovered while switch ON → re-play (backoff, log gap)

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
