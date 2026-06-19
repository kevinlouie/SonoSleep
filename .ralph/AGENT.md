# Ralph Agent Configuration

## Prerequisites
- Go >= 1.22
- `ffmpeg` on PATH (PCM s16le → MP3 encode). In Docker it is installed in the image.
- An MQTT broker reachable (Home Assistant's broker on the Synology host).
- A Home Assistant long-lived access token for REST orchestration.

## Environment (never commit real values)
Copy `.env.example` → `.env`:
```
HWN_HTTP_ADDR=:8099                     # stream + health listen addr
HWN_PUBLIC_BASE_URL=http://<host>:8099  # URL the Sonos fetches (LAN-reachable)
HWN_HA_BASE_URL=http://homeassistant.local:8123
HWN_HA_TOKEN=<long-lived-access-token>
HWN_HA_MEDIA_PLAYER=media_player.bedroom
HWN_MQTT_BROKER=tcp://<host>:1883
HWN_MQTT_USER=
HWN_MQTT_PASS=
HWN_DEFAULT_PRESET=brown
HWN_DEFAULT_VOLUME=80
```

## Build Instructions
```bash
go build ./...
```

## Test Instructions
```bash
go test ./...
```

## Run Instructions
```bash
# local (requires ffmpeg, MQTT, HA token in .env)
go run ./cmd/hwnsonos

# smoke-test the stream without HA/Sonos (writes 10s of MP3 to a file)
curl -s "http://localhost:8099/stream?preset=brown" --max-time 10 -o /tmp/brown.mp3 && ffprobe /tmp/brown.mp3

# docker
docker compose up --build -d
```

## Notes
- Update this file when the build process changes.
- DSP lives in `internal/noise`; HA client in `internal/ha`; MQTT entities in
  `internal/mqtt`; stream handler in `internal/stream`; wiring in `cmd/hwnsonos`.
- Automated tests cover DSP + payload building only. Sonos playback is verified
  manually against the live `media_player.bedroom` (see `.ralph/specs/sonos-streaming.md`).
