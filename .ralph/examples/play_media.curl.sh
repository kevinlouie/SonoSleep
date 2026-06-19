#!/usr/bin/env bash
# Example: tell the bedroom Sonos to play the brown-noise stream via the HA REST API.
# This is exactly what internal/ha will POST. Run manually to verify Sonos compat
# (Phase 2 load-bearing test) before the Go client exists.
#
# Requires: HWN_HA_BASE_URL, HWN_HA_TOKEN, HWN_PUBLIC_BASE_URL in the environment.
set -euo pipefail

: "${HWN_HA_BASE_URL:?set HWN_HA_BASE_URL e.g. http://homeassistant.local:8123}"
: "${HWN_HA_TOKEN:?set HWN_HA_TOKEN to a HA long-lived access token}"
: "${HWN_PUBLIC_BASE_URL:?set HWN_PUBLIC_BASE_URL e.g. http://192.168.1.50:8099}"

ENTITY="${1:-media_player.bedroom}"
PRESET="${2:-brown}"

# Play the infinite stream
curl -sf -X POST "${HWN_HA_BASE_URL}/api/services/media_player/play_media" \
  -H "Authorization: Bearer ${HWN_HA_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{
        \"entity_id\": \"${ENTITY}\",
        \"media_content_id\": \"x-rincon-mp3radio://${HWN_PUBLIC_BASE_URL}/stream?preset=${PRESET}\",
        \"media_content_type\": \"music\"
      }"
# NOTE: the x-rincon-mp3radio:// scheme is REQUIRED — a plain http:// URL fails with
# UPnP 714 "Illegal MIME-Type" on Sonos. Verified 2026-06-19. Keep the inner http://.

# Set a gentle volume (0.0-1.0). Bedroom — keep it low.
curl -sf -X POST "${HWN_HA_BASE_URL}/api/services/media_player/volume_set" \
  -H "Authorization: Bearer ${HWN_HA_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "{\"entity_id\": \"${ENTITY}\", \"volume_level\": 0.15}"

echo "Playing ${PRESET} on ${ENTITY}. Stop with: media_player.media_stop"
