// Package stream serves the infinite chunked MP3 endpoint that the Sonos fetches.
//
// GET /stream?preset={white|pink|brown} spawns an ffmpeg encoder, pipes
// synthesized PCM (from internal/noise) into its stdin, and copies the MP3
// stdout to the HTTP response with regular flushing. The response uses
// Icecast-radio semantics (audio/mpeg, chunked, no Content-Length) so the Sonos
// treats it as an endless radio stream rather than a finite track — see
// .ralph/specs/sonos-streaming.md (LOAD-BEARING).
//
// Lifetime is tied to the request context: when the client (Sonos) disconnects,
// the context is cancelled, exec.CommandContext SIGKILLs ffmpeg, its stdin pipe
// closes, and the feeder goroutine unblocks and exits. No goroutine or process
// leaks per connection.
package stream

import (
	"context"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"

	"github.com/kevin/ha-white-noise-sonos/internal/noise"
)

// Default encoder parameters. CBR MP3 keeps the stream rate stable, which Sonos
// prefers for long radio-style playback.
const (
	defaultBitrate = "192k"
	// blockFrames is how many stereo frames the feeder synthesizes per write.
	// 4096 frames ≈ 85 ms at 48 kHz: small enough to start fast and flush
	// smoothly, large enough to keep syscall overhead negligible.
	blockFrames = 4096
)

// Streamer is the /stream HTTP handler. Construct with New.
type Streamer struct {
	// FFmpegPath is the encoder binary (default "ffmpeg").
	FFmpegPath string
	// Bitrate is the libmp3lame CBR bitrate (default "192k").
	Bitrate string
	// DefaultPreset is used when the request omits ?preset.
	DefaultPreset noise.Preset

	// encoderCmd builds the encoder process. Overridable in tests to inject a
	// fake (e.g. `cat`) so teardown can be exercised without real ffmpeg.
	encoderCmd func(ctx context.Context) *exec.Cmd

	// onStreamEnd, if set, is called after a connection's encoder has been
	// reaped. Test hook for verifying teardown without racing on ProcessState.
	onStreamEnd func()
}

// New returns a Streamer with ffmpeg defaults. defaultPreset is served when the
// request has no ?preset query parameter.
func New(defaultPreset noise.Preset) *Streamer {
	s := &Streamer{
		FFmpegPath:    "ffmpeg",
		Bitrate:       defaultBitrate,
		DefaultPreset: defaultPreset,
	}
	s.encoderCmd = s.ffmpegCmd
	return s
}

// ffmpegCmd builds the default ffmpeg invocation: read raw s16le stereo 48 kHz
// PCM on stdin, emit CBR MP3 on stdout.
func (s *Streamer) ffmpegCmd(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, s.FFmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-f", "s16le",
		"-ar", strconv.Itoa(noise.SampleRate),
		"-ac", strconv.Itoa(noise.Channels),
		"-i", "-",
		"-c:a", "libmp3lame",
		"-b:a", s.Bitrate,
		"-f", "mp3",
		"-",
	)
}

// ServeHTTP implements the /stream endpoint.
func (s *Streamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	preset := s.DefaultPreset
	if q := r.URL.Query().Get("preset"); q != "" {
		p, err := noise.ParsePreset(q)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		preset = p
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	cmd := s.encoderCmd(ctx)
	// ffmpeg stderr → service log for diagnostics on the real Sonos. -loglevel
	// error keeps the happy path silent.
	cmd.Stderr = logWriter{}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "encoder setup failed", http.StatusInternalServerError)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		http.Error(w, "encoder setup failed", http.StatusInternalServerError)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("stream: encoder start failed: %v", err)
		http.Error(w, "encoder start failed", http.StatusInternalServerError)
		return
	}
	// Reap on return — runs after copyAndFlush drains stdout, so no
	// StdoutPipe-before-Wait deadlock.
	defer func() {
		_ = cmd.Wait()
		if s.onStreamEnd != nil {
			s.onStreamEnd()
		}
	}()

	// Feed synthesized PCM into the encoder until the client disconnects or the
	// encoder dies.
	go feed(ctx, stdin, preset)

	// Radio-stream headers. NO Content-Length → Go uses chunked encoding, which
	// makes Sonos treat this as an endless stream, not a finite track.
	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "no-cache, no-store")
	// "Connection: keep-alive" is a hop-by-hop header that Go's server manages
	// itself; setting it here is a harmless no-op, kept to document intent.
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // emit headers immediately; Sonos times out slow starts.

	copyAndFlush(w, flusher, stdout)
}

// feed synthesizes PCM and writes it to the encoder's stdin. It returns when the
// context is cancelled (client disconnect → ffmpeg killed → pipe closed) or any
// write fails (ffmpeg exited on its own). The two exit paths are independent:
// the ctx check covers idle cancellation, the write error covers ffmpeg dying
// while the client is still connected.
func feed(ctx context.Context, w io.WriteCloser, p noise.Preset) {
	defer func() { _ = w.Close() }()
	g := noise.New(p)
	buf := make([]byte, blockFrames*noise.BytesPerFrame)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		g.Fill(buf, blockFrames)
		if _, err := w.Write(buf); err != nil {
			return
		}
	}
}

// copyAndFlush streams r to w, flushing after every block so the Sonos receives
// audio with minimal latency. Returns when r reaches EOF (encoder gone) or the
// write to the client fails (client gone).
func copyAndFlush(w io.Writer, f http.Flusher, r io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			f.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

// logWriter routes encoder stderr lines to the standard logger.
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	log.Printf("ffmpeg: %s", p)
	return len(p), nil
}
