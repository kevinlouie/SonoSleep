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
- [x] `internal/mqtt`: connect, publish discovery configs for switch / select / number (see `specs/mqtt-entities.md` + `examples/`). **VERIFIED BY RUN:** added `github.com/eclipse/paho.mqtt.golang v1.5.1` via `go get` (go.mod/go.sum updated). `internal/control` is the authoritative `{on,preset,volume}` state (mutex-guarded), satisfies `ha.Controller` (IsOn/SuppressedUntil/Replay) so the watchdog reads it; deliberate stop sets a 15 s suppress window. `internal/mqtt` publishes 3 retained discovery configs (homeassistant/{switch|select|number}/hwn_sonos/.../config) device-grouped under "White Noise (Sonos)" identifiers [hwn_sonos], LWT availability `hwnsonos/status` online/offline retained. Transport abstracted behind a `Broker` interface (paho adapter + fake in tests). `cmd/hwnsonos/main.go` now wires config→ha.Client→control.State→mqtt.Connect→watchdog + the existing /healthz + /stream server; graceful shutdown publishes MQTT offline and media_stop if on. `go build/vet ./...` clean, `gofmt -l internal/mqtt internal/control cmd` empty, `go test -race ./...` ALL pass (21 new tests in mqtt+control). Coverage: discovery JSON shape (topics, device block identifiers/name/mfr/model, availability_topic, switch payload_on/off, select options order, number min/max/step/unit), command→action mapping via fake player+broker (ON→play, OFF→stop, preset re-play-when-on / no-replay-when-off / invalid-rejected, volume set+clamp+non-int-ignored), OnConnect publishes discovery+online+state, reconcile re-asserts authoritative play_media (pink/55) not stale retained, PublishOffline retained; control suppress-window-on-stop with injected clock.
- [x] Subscribe command topics; map: switch ON/OFF → play/stop; select → preset (re-play if on); number → volume_set
- [x] Publish state topics + `availability` (online/offline LWT); reconcile on reconnect
- [x] Unit-test discovery payload JSON shape

## Phase 5 — Hardening & docs
- [x] Backoff/jitter on all reconnect loops; structured logging
- [x] Graceful shutdown (stop stream, media_stop, MQTT offline LWT)
- [x] README: setup, env, HA token creation, add MQTT integration, troubleshooting (714 MIME, drops)
- [x] Optional: fade-in on start (ramp gain ~3 s) to match reSpeakerSleep behavior

## Completed
- [x] Project enabled for Ralph
- [x] Research prior art; architecture decided (see `projectplan.md`)
- [x] Ralph files + specs written

## Phase 5 — VERIFIED BY RUN (2026-06-20)
- **Backoff/jitter + slog.** Audited all retry paths. `ha.waitAvailable` already had capped
  exponential backoff + jitter; extracted the calc into a pure `backoffDelay(attempt, base,
  max, jitter)` (internal/ha/util.go) so it's unit-tested (3 tests: exponential schedule,
  jitter bounds [0.5·step, step], out-of-range clamp). MQTT reconnect uses paho's
  auto-reconnect — added `SetMaxReconnectInterval(2m)` to cap its internal exponential
  backoff and jittered the initial connect-retry interval (5s ±5s). Watchdog re-play is
  ticker-gated (no tight loop) — left as-is. Migrated ALL `log.Printf`/`log.Fatalf` to
  `log/slog` (key/value attrs) across ha, mqtt, stream, control wiring, main. New
  `internal/logging` package: `Setup` installs a process-wide TextHandler at a configurable
  level; `ParseLevel` parses HWN_LOG_LEVEL (debug|info|warn|error, default info) — unit-tested.
- **Graceful shutdown.** Reordered main's shutdown into an explicit bounded sequence
  (10s timeout): srv.Shutdown → MQTT PublishOffline → media_stop (if ON, reading ctrl state
  before teardown) → mqttDisconnect (paho re-publishes offline + clean close). LWT still
  covers ungraceful drops. No more reliance on defer ordering.
- **README** rewritten as a full operator guide: what/architecture diagram/playback flow,
  env table (→ .env.example), HA long-lived-token steps, adding the MQTT integration, the
  three entities + topics (verified against internal/mqtt), Synology docker-compose deploy
  (root compose, host network), and a Troubleshooting section covering UPnP 714 +
  x-rincon-mp3radio:// fix, stream drops/watchdog, speaker unavailable, ffmpeg missing,
  MQTT not connecting.
- **Fade-in DONE (not deferred).** Judged low-risk: added an opt-in linear gain envelope to
  `noise.Generator` via `NewWithFade(preset, FadeInDuration=3s)`; `noise.New` stays
  fade-free so existing RMS/spectral tests are untouched. `stream.feed` uses the faded
  constructor. 4 new envelope tests (no-fade default, ramp 0→0.5→1, early<steady RMS with
  steady ≈ un-faded level, zero/negative disables).
- `go build/vet ./...` clean, `gofmt -l .` empty, `go test ./...` + `go test -race ./...`
  ALL pass (7 packages incl. new internal/logging).

## Notes
- The Phase 2 manual Sonos test is the single biggest risk — do it before building
  Phases 3–4 in full. If the stream won't hold, revisit format (FLAC) or add ICY headers.
- Keep `projectplan.md` decisions log updated with anything learned about Sonos behavior.
