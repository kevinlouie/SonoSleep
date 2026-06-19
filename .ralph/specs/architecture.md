# Spec: Architecture

See `projectplan.md` for the full rationale; this is the implementation-facing summary.

## Components (Go packages)
- `cmd/hwnsonos` — main; loads config, starts HTTP server, MQTT client, watchdog.
- `internal/config` — env loading + validation.
- `internal/noise` — white/pink/brown PCM generators (s16le, stereo, 48 kHz).
- `internal/stream` — HTTP handler; PCM → ffmpeg → chunked MP3 per connection.
- `internal/ha` — Home Assistant REST client (play_media, volume_set, media_stop, get_state).
- `internal/mqtt` — HA MQTT-discovery entities (switch/select/number) + command/state handling.
- `internal/control` (optional) — shared state machine (current preset, volume, on/off) that
  both MQTT and the watchdog read/write. Single source of truth; guard with a mutex.

## State machine
Authoritative state held in the service:
```
on:      bool     // switch
preset:  white|pink|brown
volume:  0..100
```
Transitions:
- switch ON  → set on=true; HA play_media(stream?preset) + volume_set(volume)
- switch OFF → set on=false; HA media_stop
- preset Δ   → if on: re-play with new preset URL (stop then play, or play_media replaces)
- volume Δ   → HA volume_set(volume/100); also affects newly generated samples if gain is server-side
- watchdog: Sonos went idle/paused while on==true → re-play (backoff)

Volume note: prefer **Sonos-side** volume (`volume_set`) so generated PCM stays full-scale
(better MP3 SNR). Server-side gain only if a future need arises.

## Concurrency
- One generator goroutine + one ffmpeg process **per stream connection**. Normally exactly
  one connection (the Sonos). Tie their lifetime to the request context; kill ffmpeg and
  stop the generator on disconnect.
- Watchdog runs on a ticker (e.g. 10 s) or via HA WebSocket state subscription.
- All HA REST calls and MQTT publishes funnel through the `control` state to avoid races.
- **Backpressure:** the generator MUST pace itself on ffmpeg's blocking stdin write (and in
  turn the TCP write to Sonos). Do NOT pre-buffer generated PCM ahead of the encoder — if
  the Sonos stalls, an unbounded buffer leaks memory. Generate one block, write it (blocks
  until consumed), repeat.

## Failure handling
- HA target `unavailable` (speaker off): do not spin; backoff and report via MQTT state.
- ffmpeg crash mid-stream: handler returns; Sonos reconnects → watchdog/connection restarts it.
- MQTT broker down: retry with backoff; LWT marks entity `offline`.
