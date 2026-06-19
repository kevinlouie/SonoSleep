package stream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/kevin/ha-white-noise-sonos/internal/noise"
)

// TestServeHTTP_RealFFmpeg exercises the default ffmpeg encoder end-to-end and
// asserts the body is actual MP3 (frame sync 0xFF 0xEx/0xFx, or an ID3 tag).
// Skips when ffmpeg is not installed.
func TestServeHTTP_RealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	srv := httptest.NewServer(New(noise.Brown))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stream?preset=brown", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 8192)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read MP3 body: %v", err)
	}
	id3 := buf[0] == 'I' && buf[1] == 'D' && buf[2] == '3'
	sync := buf[0] == 0xFF && (buf[1]&0xE0) == 0xE0
	if !id3 && !sync {
		t.Fatalf("body is not MP3: first bytes %x %x %x", buf[0], buf[1], buf[2])
	}
}

// catStreamer returns a Streamer whose encoder is `cat`: it copies PCM stdin to
// stdout unchanged. That lets us exercise the HTTP plumbing and teardown without
// depending on ffmpeg being installed.
func catStreamer(t *testing.T) *Streamer {
	t.Helper()
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}
	s := New(noise.Brown)
	s.encoderCmd = func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "cat")
	}
	return s
}

func TestServeHTTP_BadPreset(t *testing.T) {
	s := catStreamer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream?preset=purple", nil)
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("preset=purple: got %d, want 400", rec.Code)
	}
}

func TestServeHTTP_HeadersAndStream(t *testing.T) {
	s := catStreamer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stream?preset=brown", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}
	if resp.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1 (chunked, unknown)", resp.ContentLength)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache, no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	// Must produce bytes promptly (Sonos times out slow starts).
	buf := make([]byte, 4096)
	n, err := io.ReadFull(resp.Body, buf)
	if err != nil {
		t.Fatalf("read body: got %d bytes, err %v", n, err)
	}
	if n != len(buf) {
		t.Fatalf("short read: %d", n)
	}
}

// TestServeHTTP_Teardown verifies the handler returns and the encoder process is
// gone after the client disconnects mid-stream — the "no leaks" requirement.
func TestServeHTTP_Teardown(t *testing.T) {
	s := catStreamer(t)

	// Capture the spawned command, and signal when the handler has reaped it.
	var spawned *exec.Cmd
	inner := s.encoderCmd
	s.encoderCmd = func(ctx context.Context) *exec.Cmd {
		spawned = inner(ctx)
		return spawned
	}
	done := make(chan struct{})
	s.onStreamEnd = func() { close(done) }

	srv := httptest.NewServer(s)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	// Pull a little data to ensure streaming is underway, then disconnect.
	buf := make([]byte, 1024)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("initial read: %v", err)
	}
	cancel()
	resp.Body.Close()

	// The encoder (cat) should be SIGKILLed via CommandContext and reaped by the
	// handler's deferred Wait. onStreamEnd fires after that, establishing a
	// happens-before so reading ProcessState below is race-free.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("encoder process not torn down after client disconnect")
	}
	if spawned.ProcessState == nil {
		t.Fatal("encoder ProcessState nil after teardown")
	}
}
