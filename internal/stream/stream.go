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
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
	// defaultBurst is how far ahead of real time the feeder may run: an
	// Icecast-style burst-on-connect so the Sonos fills its start-up buffer
	// immediately, then real time. 10 s keeps start latency the same as
	// unpaced (a 1 s burst made the Sonos visibly slower to start) while no
	// longer pushing minutes of audio into each connection. (The start-up blip
	// is the Sonos's own and is handled by muting in ha.PlayMedia, not here.)
	defaultBurst = 10 * time.Second
)

// Streamer is the /stream HTTP handler. Construct with New.
type Streamer struct {
	// FFmpegPath is the encoder binary (default "ffmpeg").
	FFmpegPath string
	// Bitrate is the libmp3lame CBR bitrate (default "192k").
	Bitrate string
	// DefaultPreset is used when the request omits ?preset.
	DefaultPreset noise.Preset
	// FadeIn is the gain ramp applied at the start of each connection
	// (default noise.FadeInDuration). Zero disables it.
	FadeIn time.Duration
	// Burst is how much audio may be sent ahead of real time (default
	// defaultBurst). After that the feeder is paced to real time. Negative
	// disables pacing (tests only).
	Burst time.Duration

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
		FadeIn:        noise.FadeInDuration,
		Burst:         defaultBurst,
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
		slog.Error("stream: encoder start failed", "err", err)
		http.Error(w, "encoder start failed", http.StatusInternalServerError)
		return
	}
	slog.Info("stream: client connected", "preset", preset.String(), "remote", r.RemoteAddr)
	start := time.Now()
	var sent int64
	// Reap on return — runs after copyAndFlush drains stdout, so no
	// StdoutPipe-before-Wait deadlock.
	defer func() {
		_ = cmd.Wait()
		slog.Info("stream: client disconnected", "preset", preset.String(), "remote", r.RemoteAddr,
			"duration", time.Since(start).Round(time.Millisecond), "bytes", sent)
		if s.onStreamEnd != nil {
			s.onStreamEnd()
		}
	}()

	// Feed synthesized PCM into the encoder until the client disconnects or the
	// encoder dies.
	go feed(ctx, stdin, preset, s.FadeIn, s.Burst)

	// Radio-stream headers. NO Content-Length → Go uses chunked encoding, which
	// makes Sonos treat this as an endless stream, not a finite track.
	h := w.Header()
	h.Set("Content-Type", "audio/mpeg")
	h.Set("Cache-Control", "no-cache, no-store")
	// ICY (Shoutcast/Icecast) station metadata. Sonos reads icy-name as the
	// displayed source name; without it the app shows "Unknown Content".
	h.Set("icy-name", stationName(preset))
	h.Set("icy-genre", "White noise")
	h.Set("icy-br", "192")
	// "Connection: keep-alive" is a hop-by-hop header that Go's server manages
	// itself; setting it here is a harmless no-op, kept to document intent.
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // emit headers immediately; Sonos times out slow starts.

	sent = copyAndFlush(w, flusher, stdout)
}

// stationName is the human-friendly name shown in the Sonos app (ICY icy-name),
// e.g. "Brown Noise (Local)".
func stationName(p noise.Preset) string {
	switch p {
	case noise.White:
		return "White Noise (Local)"
	case noise.Pink:
		return "Pink Noise (Local)"
	case noise.Brown:
		return "Brown Noise (Local)"
	default:
		return "Noise (Local)"
	}
}

// feed synthesizes PCM and writes it to the encoder's stdin. It returns when the
// context is cancelled (client disconnect → ffmpeg killed → pipe closed) or any
// write fails (ffmpeg exited on its own). The two exit paths are independent:
// the ctx check covers idle cancellation, the write error covers ffmpeg dying
// while the client is still connected.
//
// Pacing: after an initial burst of `burst` audio, PCM is produced no faster
// than real time (the encoder adds only a few frames of latency, so the MP3
// leaving the server is paced too). A negative burst disables pacing.
func feed(ctx context.Context, w io.WriteCloser, p noise.Preset, fade, burst time.Duration) {
	defer func() { _ = w.Close() }()
	// Ease playback in with a gain ramp so the Sonos doesn't snap from silence
	// to full level once its start-up buffering finishes.
	g := noise.NewWithFade(p, fade)
	buf := make([]byte, blockFrames*noise.BytesPerFrame)
	start := time.Now()
	var frames int64 // frames written so far
	for {
		if burst >= 0 {
			// Audio already written covers frames/SampleRate seconds; it may lead
			// the wall clock by at most burst.
			// Split seconds and remainder so the Duration math can't overflow on
			// a stream left running for days.
			written := time.Duration(frames/noise.SampleRate)*time.Second +
				time.Duration(frames%noise.SampleRate)*time.Second/noise.SampleRate
			lead := written - burst - time.Since(start)
			if lead > 0 {
				t := time.NewTimer(lead)
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		g.Fill(buf, blockFrames)
		if _, err := w.Write(buf); err != nil {
			return
		}
		frames += blockFrames
	}
}

// copyAndFlush streams r to w, flushing after every block so the Sonos receives
// audio with minimal latency. Returns the bytes delivered when r reaches EOF
// (encoder gone) or the write to the client fails (client gone).
func copyAndFlush(w io.Writer, f http.Flusher, r io.Reader) int64 {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total
			}
			total += int64(n)
			f.Flush()
		}
		if rerr != nil {
			return total
		}
	}
}

// logWriter routes encoder stderr lines to the structured logger at warn level
// (ffmpeg runs with -loglevel error, so any output here is a real problem).
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	slog.Warn("ffmpeg", "msg", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
