// Command hwnsonos synthesizes continuous brown/pink/white noise and streams it
// to a Sonos speaker via Home Assistant. It serves an infinite chunked-MP3
// /stream endpoint (fetched by the Sonos), exposes HA control entities over MQTT
// discovery (switch/select/number), drives playback through the HA REST API, and
// runs a watchdog that re-plays if the Sonos drops the stream.
//
// Wiring: config → ha.Client → control.State (authoritative; its Run goroutine is
// the only thing that calls HA) → mqtt.Service → ha.Watchdog, plus the /healthz +
// /stream HTTP server. Graceful shutdown stops playback if it was on, closes the
// streams, publishes MQTT offline and disconnects.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kevin/ha-white-noise-sonos/internal/config"
	"github.com/kevin/ha-white-noise-sonos/internal/control"
	"github.com/kevin/ha-white-noise-sonos/internal/ha"
	"github.com/kevin/ha-white-noise-sonos/internal/logging"
	"github.com/kevin/ha-white-noise-sonos/internal/mqtt"
	"github.com/kevin/ha-white-noise-sonos/internal/noise"
	"github.com/kevin/ha-white-noise-sonos/internal/stream"
)

// Shutdown time budget. The steps run in sequence: reconciler stop (≤5 s — an HA
// service call already sent is allowed to finish, bounded by ha's 4 s
// serviceCallTimeout, so the wait covers it and the reconciler has recorded the
// result before NeedsFinalStop is read) + media_stop (≤4 s, the service-call
// bound) + HTTP close (≤2 s) + MQTT offline and disconnect (≤2 s write timeout +
// 1 s token wait + 0.25 s, see internal/mqtt) ≈ 14.25 s worst case, inside
// docker-compose.yml's 20 s stop_grace_period. The common case (no HA call in
// flight) is well under 10 s.
const (
	reconcilerStopTimeout = 5 * time.Second
	mediaStopTimeout      = 5 * time.Second
	httpShutdownTimeout   = 2 * time.Second
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Logger not yet configured; use slog's default (stderr) and exit.
		slog.Error("startup: config load failed", "err", err)
		os.Exit(1)
	}

	_, levelOK := logging.Setup(cfg.LogLevel)
	if !levelOK {
		slog.Warn("unrecognized HWN_LOG_LEVEL, defaulting to info", "value", cfg.LogLevel)
	}

	// Config already validated DefaultPreset against white|pink|brown.
	defaultPreset, _ := noise.ParsePreset(cfg.DefaultPreset)

	// HA REST client → authoritative control state → watchdog uses the same state.
	haClient := ha.New(cfg.HABaseURL, cfg.HAToken, cfg.HAMediaPlayer, cfg.PublicBaseURL)
	ctrl := control.New(haClient, cfg.DefaultPreset, cfg.DefaultVolume)
	watchdog := ha.NewWatchdog(haClient, ctrl, 0)

	// HTTP server: /healthz + /stream.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	streamer := stream.New(defaultPreset)
	streamer.FadeIn = time.Duration(cfg.FadeInSeconds) * time.Second
	mux.Handle("/stream", streamer)

	// Every request context derives from streamsCtx, so cancelling it on
	// shutdown ends the otherwise-infinite /stream handlers (srv.Shutdown alone
	// would wait for them forever).
	streamsCtx, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()
	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: mux,
		// No write timeout: the stream handler holds the connection open
		// indefinitely. Read header timeout guards against slow-loris on requests.
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return streamsCtx },
	}

	// Shut down cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Reconciler: the only goroutine that issues HA calls for the control state.
	reconcilerDone := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(reconcilerDone)
	}()

	// MQTT: discovery entities + command/state handling. Connect wires the
	// on-connect/reconnect reconcile (re-publish discovery, re-assert state).
	_, mqttDisconnect, err := mqtt.Connect(ctx, cfg.MQTTBroker, cfg.MQTTUser, cfg.MQTTPass, ctrl)
	if err != nil {
		slog.Error("mqtt: connect failed", "err", err)
		os.Exit(1)
	}

	// Watchdog: re-play if the Sonos drops the stream while the switch is ON.
	go watchdog.Run(ctx)

	go func() {
		slog.Info("hwnsonos listening",
			"addr", cfg.HTTPAddr, "preset", cfg.DefaultPreset,
			"volume", cfg.DefaultVolume, "target", cfg.HAMediaPlayer)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	stop() // restore default signal handling; a second signal now force-quits.

	// Bounded, ordered shutdown:
	//  1. Wait for the reconciler to exit (ctx is cancelled, which abandons any
	//     backoff; a service call already sent finishes) so the final media_stop
	//     can't race a play.
	//  2. If the switch is ON, or we started playback that no media_stop has
	//     ended (e.g. an OFF still pending), media_stop FIRST so the Sonos drops
	//     the stream itself instead of hitting a closed connection and erroring.
	//  3. Cancel every /stream handler and shut the HTTP server down, falling
	//     back to a hard Close if anything is still open.
	//  4. Publish MQTT "offline" (once) and disconnect.
	select {
	case <-reconcilerDone:
	case <-time.After(reconcilerStopTimeout):
		slog.Warn("shutdown: reconciler did not stop in time")
	}

	if ctrl.NeedsFinalStop() {
		stopCtx, cancel := context.WithTimeout(context.Background(), mediaStopTimeout)
		if err := haClient.MediaStop(stopCtx); err != nil {
			slog.Error("shutdown: media_stop", "err", err)
		}
		cancel()
	}

	closeHTTP(srv, cancelStreams, httpShutdownTimeout)

	mqttDisconnect()
	slog.Info("shutdown complete")
}

// closeHTTP ends every in-flight request (cancelStreams cancels the server's
// BaseContext, which the never-ending /stream handlers watch), then shuts srv
// down within timeout, falling back to a hard Close.
func closeHTTP(srv *http.Server, cancelStreams context.CancelFunc, timeout time.Duration) {
	cancelStreams()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Warn("shutdown: http server did not drain, closing", "err", err)
		_ = srv.Close()
	}
}
