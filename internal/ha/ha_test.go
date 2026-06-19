package ha

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// newTestClient wires a Client to a test server, with fast deterministic backoff
// (no real sleeps) so retry paths run instantly.
func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c := New(baseURL, "test-token", "media_player.bedroom", "http://host:8099")
	c.baseDelay = time.Millisecond
	c.maxDelay = time.Millisecond
	c.jitter = func() float64 { return 0 }
	c.sleep = func(context.Context, time.Duration) error { return nil } // never actually sleep
	return c
}

// stateServer returns a server that reports `state` for GET /api/states/... and
// records every POST /api/services/... call.
type capturedCall struct {
	method string
	path   string
	auth   string
	ctype  string
	body   map[string]any
}

func TestPlayMedia_RequestShape(t *testing.T) {
	var mu sync.Mutex
	var calls []capturedCall

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"state":"playing"}`)
			return
		}
		body := decodeBody(t, r)
		mu.Lock()
		calls = append(calls, capturedCall{
			method: r.Method, path: r.URL.Path,
			auth: r.Header.Get("Authorization"), ctype: r.Header.Get("Content-Type"),
			body: body,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "[]")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.PlayMedia(context.Background(), "brown", 42); err != nil {
		t.Fatalf("PlayMedia: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("want 2 service calls (play_media, volume_set), got %d", len(calls))
	}

	// play_media
	pm := calls[0]
	if pm.method != http.MethodPost {
		t.Errorf("play_media method = %q, want POST", pm.method)
	}
	if pm.path != "/api/services/media_player/play_media" {
		t.Errorf("play_media path = %q", pm.path)
	}
	if pm.auth != "Bearer test-token" {
		t.Errorf("play_media auth = %q, want Bearer test-token", pm.auth)
	}
	if pm.ctype != "application/json" {
		t.Errorf("play_media content-type = %q", pm.ctype)
	}
	if got := pm.body["entity_id"]; got != "media_player.bedroom" {
		t.Errorf("entity_id = %v", got)
	}
	wantID := "x-rincon-mp3radio://http://host:8099/stream?preset=brown"
	if got := pm.body["media_content_id"]; got != wantID {
		t.Errorf("media_content_id = %v, want %v", got, wantID)
	}
	if got := pm.body["media_content_type"]; got != "music" {
		t.Errorf("media_content_type = %v, want music", got)
	}

	// volume_set
	vs := calls[1]
	if vs.path != "/api/services/media_player/volume_set" {
		t.Errorf("volume_set path = %q", vs.path)
	}
	if got, ok := vs.body["volume_level"].(float64); !ok || got != 0.42 {
		t.Errorf("volume_level = %v, want 0.42", vs.body["volume_level"])
	}
}

func TestMediaContentID_RinconPrefix(t *testing.T) {
	c := New("http://ha:8123", "tok", "media_player.bedroom", "http://host:8099/")
	got := c.MediaContentID("pink")
	want := "x-rincon-mp3radio://http://host:8099/stream?preset=pink"
	if got != want {
		t.Fatalf("MediaContentID = %q, want %q", got, want)
	}
}

func TestVolumeSet_Clamp(t *testing.T) {
	cases := []struct {
		in   int
		want float64
	}{{-10, 0.0}, {0, 0.0}, {50, 0.5}, {100, 1.0}, {150, 1.0}}
	for _, tc := range cases {
		var got float64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := decodeBody(t, r)
			got, _ = b["volume_level"].(float64)
			w.WriteHeader(http.StatusOK)
		}))
		c := newTestClient(t, srv.URL)
		if err := c.VolumeSet(context.Background(), tc.in); err != nil {
			t.Fatalf("VolumeSet(%d): %v", tc.in, err)
		}
		srv.Close()
		if got != tc.want {
			t.Errorf("VolumeSet(%d) → volume_level %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMediaStop_RequestShape(t *testing.T) {
	var got capturedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = capturedCall{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: decodeBody(t, r)}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.MediaStop(context.Background()); err != nil {
		t.Fatalf("MediaStop: %v", err)
	}
	if got.method != http.MethodPost || got.path != "/api/services/media_player/media_stop" {
		t.Errorf("media_stop call = %s %s", got.method, got.path)
	}
	if got.auth != "Bearer test-token" {
		t.Errorf("media_stop auth = %q", got.auth)
	}
	if got.body["entity_id"] != "media_player.bedroom" {
		t.Errorf("media_stop entity_id = %v", got.body["entity_id"])
	}
}

func TestGetState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states/media_player.bedroom" {
			t.Errorf("get_state path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("get_state auth = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"state":"idle","attributes":{}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	st, err := c.GetState(context.Background())
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if st != "idle" {
		t.Errorf("GetState = %q, want idle", st)
	}
}

func TestPlayMedia_BackoffOnUnavailable_ThenAvailable(t *testing.T) {
	var mu sync.Mutex
	stateCalls := 0
	playCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			stateCalls++
			n := stateCalls
			mu.Unlock()
			// First two probes: unavailable; then it comes online.
			if n <= 2 {
				_, _ = io.WriteString(w, `{"state":"unavailable"}`)
			} else {
				_, _ = io.WriteString(w, `{"state":"playing"}`)
			}
			return
		}
		mu.Lock()
		playCalled = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	sleeps := 0
	c.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }

	if err := c.PlayMedia(context.Background(), "white", 30); err != nil {
		t.Fatalf("PlayMedia: %v", err)
	}
	if stateCalls != 3 {
		t.Errorf("state probes = %d, want 3", stateCalls)
	}
	if sleeps != 2 {
		t.Errorf("backoff sleeps = %d, want 2", sleeps)
	}
	if !playCalled {
		t.Error("play_media never issued after recovery")
	}
}

func TestPlayMedia_StaysUnavailable_ReturnsErrUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"state":"unavailable"}`)
			return
		}
		t.Error("play_media should not be called while unavailable")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	c.maxRetries = 3
	err := c.PlayMedia(context.Background(), "brown", 50)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("PlayMedia err = %v, want ErrUnavailable", err)
	}
}

func TestCallService_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Illegal MIME-Type 714", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.VolumeSet(context.Background(), 50)
	if err == nil {
		t.Fatal("want error on 500, got nil")
	}
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	data, _ := io.ReadAll(r.Body)
	if len(data) == 0 {
		return m
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode body %q: %v", data, err)
	}
	return m
}
