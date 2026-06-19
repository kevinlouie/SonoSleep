# Spec: Sonos streaming (LOAD-BEARING constraints)

The whole architecture rests on the Sonos accepting and holding an infinite HTTP audio
stream. These rules come from researching real Sonos failures (UPnP 714 MIME errors;
~300 s drops on streams served as finite tracks). Treat them as hard requirements until
disproven against the real `media_player.bedroom`.

## Stream endpoint contract
`GET /stream?preset={white|pink|brown}`

Response MUST:
- Set `Content-Type: audio/mpeg` (MP3). Wrong/missing type → Sonos UPnP **714 "Illegal
  MIME-Type"**.
- Use **chunked transfer encoding** (Go default when you don't set Content-Length and
  flush). **Never** set `Content-Length` — a finite length makes Sonos treat it as a
  track and stop/drop near the declared/implied end.
- `Cache-Control: no-cache, no-store`, `Connection: keep-alive`.
- Begin emitting audio bytes immediately (Sonos times out slow starts). Flush after each
  block (`http.Flusher`).
- Keep streaming until the client disconnects; then tear down ffmpeg + generator.

Encoder: `ffmpeg -hide_banner -loglevel error -f s16le -ar 48000 -ac 2 -i - -c:a
libmp3lame -b:a 192k -f mp3 -` (CBR for stable streaming; tune bitrate later).
Optionally prepend ICY headers (`icy-name`, `icy-br`) if drop tests show Sonos prefers
radio-style streams.

## play_media call (from the Go HA client) — VERIFIED 2026-06-19
**A plain `http://` URL with `media_content_type: music` FAILS with UPnP 714 "Illegal
MIME-Type"** on this Sonos — confirmed against the live kitchen speaker even with a
known-good Icecast MP3 radio stream (SomaFM). The fix that works is the Sonos
**`x-rincon-mp3radio://` URI scheme** prefix, which makes Sonos treat the URL as an MP3
radio stream and skips the MIME check:
```jsonc
// POST /api/services/media_player/play_media
{
  "entity_id": "media_player.bedroom",
  "media_content_id": "x-rincon-mp3radio://http://<host>:<port>/stream?preset=brown",
  "media_content_type": "music"
}
```
- Keep the inner `http://` after the scheme — verified working form is
  `x-rincon-mp3radio://http://...` (Sonos reports it back exactly that way).
- `media_content_type: music` is still the type to send.
- The URL host MUST be LAN-reachable by the Sonos (the Synology host IP/hostname, not
  `localhost`). Set via `HWN_PUBLIC_BASE_URL`.
- The Go HA client builds: `"x-rincon-mp3radio://" + HWN_PUBLIC_BASE_URL + "/stream?preset=" + preset`.

## Reconnect / watchdog
Sonos may still drop very long streams. Strategy:
- Watch the target `media_player` state (HA WebSocket subscription preferred; ticker poll
  acceptable). While the service switch is ON, any transition to `idle`/`paused`, or a
  recovery from `unavailable`, triggers a re-`play_media`.
- Apply backoff + jitter; log the gap. Expected re-buffer gap ≈ 1–2 s (acceptable for sleep).
- Do NOT reconnect when the switch is OFF (that's an intended stop).
- **Only reconnect on _unexpected_ idle.** Suppress the watchdog for N seconds (e.g. 15 s)
  after any deliberate stop, and after a known interruption. The bedroom Sonos may receive
  TTS/alarm announcements or be paused manually — re-playing then would stomp the
  announcement or fight the user. Track "we are intentionally not playing right now" in the
  control state and gate the watchdog on it.

## MANUAL verification checklist (human, against live speaker)
1. Power on the bedroom Sonos (currently `unavailable`).
2. Low volume (bedroom!). `media_player.play_media` the `/stream?preset=brown` URL.
3. Confirm audio + no 714 error in HA logs.
4. Let it run 60+ min. Note if/when it drops and the interval (informs watchdog timing).
5. Try preset switch and Sonos volume change while playing.
Record findings in `projectplan.md` decisions log.
