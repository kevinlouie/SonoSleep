// Package ha is a small Home Assistant REST client that orchestrates playback on
// the target Sonos media_player. It exposes the handful of service calls the rest
// of the service needs — PlayMedia, VolumeSet, MediaStop — plus GetState for the
// watchdog (internal/ha/watchdog.go).
//
// Authentication is a long-lived access token sent as a Bearer header
// (config HWN_HA_TOKEN). The base URL and target entity also come from config.
//
// Sonos quirk (LOAD-BEARING, verified live — see .ralph/specs/sonos-streaming.md):
// a plain http:// media_content_id fails with UPnP 714 "Illegal MIME-Type". The
// fix is to prefix the URL with the x-rincon-mp3radio:// scheme so Sonos treats it
// as an MP3 radio stream and skips the MIME check. PlayMedia builds that URL.
package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// rincon is the Sonos URI scheme prefix that makes the speaker accept an MP3
// radio stream without the 714 MIME check. The inner http:// is kept intact.
const rincon = "x-rincon-mp3radio://"

// State values reported by Home Assistant for a media_player entity that this
// service treats specially.
const (
	StateUnavailable = "unavailable"
	StateIdle        = "idle"
	StatePaused      = "paused"
	StatePlaying     = "playing"
)

// ErrUnavailable is returned (wrapped) when the target media_player reports the
// "unavailable" state — typically the Sonos is powered off. Callers should
// surface this rather than spin: see PlayMedia's backoff and the watchdog.
var ErrUnavailable = errors.New("ha: target media_player is unavailable")

// Client talks to the Home Assistant REST API for one target media_player entity.
// It is safe for concurrent use (http.Client is, and Client holds no mutable
// state). Construct with New.
type Client struct {
	baseURL    string // HA REST base, e.g. http://homeassistant.local:8123 (no trailing /)
	token      string // long-lived access token (Bearer)
	entityID   string // target media_player, e.g. media_player.bedroom
	publicBase string // LAN-reachable base URL the Sonos fetches, e.g. http://host:8099
	httpClient *http.Client

	// retry policy for PlayMedia when the target is unavailable. Exposed as
	// fields so tests can shrink the delays and keep runs fast.
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration
	// jitter returns a fraction in [0,1) applied to each backoff delay. Injectable
	// so tests are deterministic; defaults to math/rand.
	jitter func() float64
	// sleep blocks for d or until ctx is done. Injectable for fast tests.
	sleep func(ctx context.Context, d time.Duration) error
}

// New returns a Client for the given HA base URL, token, target entity, and the
// public base URL the Sonos fetches the stream from. baseURL and publicBase have
// any trailing slash trimmed.
func New(baseURL, token, entityID, publicBase string) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		entityID:   entityID,
		publicBase: strings.TrimRight(publicBase, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		maxRetries: 5,
		baseDelay:  500 * time.Millisecond,
		maxDelay:   30 * time.Second,
		jitter:     defaultJitter,
		sleep:      sleepCtx,
	}
	return c
}

// EntityID returns the target media_player entity id.
func (c *Client) EntityID() string { return c.entityID }

// MediaContentID builds the media_content_id for the given preset:
//
//	x-rincon-mp3radio://<PublicBaseURL>/stream?preset=<preset>
//
// The x-rincon-mp3radio:// prefix is REQUIRED for Sonos (UPnP 714 otherwise).
func (c *Client) MediaContentID(preset string) string {
	return rincon + c.publicBase + "/stream?preset=" + preset
}

// PlayMedia starts playback of the noise stream for preset, then sets the Sonos
// volume. It first probes GetState: if the target is unavailable it retries with
// capped exponential backoff + jitter; if still unavailable after maxRetries it
// returns ErrUnavailable (wrapped) so the caller can surface the state instead of
// spinning forever. volume is 0-100 and is converted to HA's 0.0-1.0 scale.
func (c *Client) PlayMedia(ctx context.Context, preset string, volume int) error {
	if err := c.waitAvailable(ctx); err != nil {
		return err
	}

	body := map[string]string{
		"entity_id":          c.entityID,
		"media_content_id":   c.MediaContentID(preset),
		"media_content_type": "music",
	}
	if err := c.callService(ctx, "media_player", "play_media", body); err != nil {
		return fmt.Errorf("ha: play_media: %w", err)
	}
	if err := c.VolumeSet(ctx, volume); err != nil {
		return err
	}
	return nil
}

// VolumeSet sets the target's volume. level is 0-100 and is mapped to HA's
// volume_level 0.0-1.0.
func (c *Client) VolumeSet(ctx context.Context, level int) error {
	if level < 0 {
		level = 0
	} else if level > 100 {
		level = 100
	}
	body := map[string]any{
		"entity_id":    c.entityID,
		"volume_level": float64(level) / 100.0,
	}
	if err := c.callService(ctx, "media_player", "volume_set", body); err != nil {
		return fmt.Errorf("ha: volume_set: %w", err)
	}
	return nil
}

// MediaStop stops playback on the target.
func (c *Client) MediaStop(ctx context.Context) error {
	body := map[string]string{"entity_id": c.entityID}
	if err := c.callService(ctx, "media_player", "media_stop", body); err != nil {
		return fmt.Errorf("ha: media_stop: %w", err)
	}
	return nil
}

// GetState returns the current state string of the target media_player (e.g.
// "playing", "idle", "paused", "unavailable") via GET /api/states/<entity_id>.
func (c *Client) GetState(ctx context.Context) (string, error) {
	url := c.baseURL + "/api/states/" + c.entityID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("ha: build get_state request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ha: get_state: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("ha: get_state read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ha: get_state status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	var st struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", fmt.Errorf("ha: get_state decode: %w", err)
	}
	return st.State, nil
}

// callService POSTs a JSON body to /api/services/<domain>/<service>.
func (c *Client) callService(ctx context.Context, domain, service string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := c.baseURL + "/api/services/" + domain + "/" + service
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// waitAvailable probes GetState and, while the target is unavailable, retries
// with capped exponential backoff + jitter (see backoffDelay). It returns nil as
// soon as the target is in any non-unavailable state, ErrUnavailable if it is
// still unavailable after maxRetries, or the context error / underlying error
// otherwise.
func (c *Client) waitAvailable(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		state, err := c.GetState(ctx)
		if err != nil {
			return err
		}
		if state != StateUnavailable {
			if attempt > 0 {
				slog.Info("ha: target became available", "entity", c.entityID, "attempts", attempt+1)
			}
			return nil
		}
		if attempt >= c.maxRetries {
			return fmt.Errorf("%w (after %d attempts)", ErrUnavailable, attempt+1)
		}

		d := backoffDelay(attempt, c.baseDelay, c.maxDelay, c.jitter())
		slog.Warn("ha: target unavailable, backing off",
			"entity", c.entityID, "attempt", attempt+1, "max_retries", c.maxRetries, "delay", d)
		if err := c.sleep(ctx, d); err != nil {
			return err
		}
	}
}
