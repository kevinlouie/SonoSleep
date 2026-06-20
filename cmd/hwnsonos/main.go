// Command hwnsonos synthesizes continuous brown/pink/white noise and streams it
// to a Sonos speaker via Home Assistant. It serves an infinite chunked-MP3
// /stream endpoint (fetched by the Sonos), exposes HA control entities over MQTT
// discovery (switch/select/number), drives playback through the HA REST API, and
// runs a watchdog that re-plays if the Sonos drops the stream.
//
// Wiring: config → ha.Client → control.State (authoritative) → mqtt.Service →
// ha.Watchdog, plus the /healthz + /stream HTTP server. Graceful shutdown
// publishes MQTT offline and stops playback if it was on.
package main

import (
	"context"
	"errors"
	"log/slog"
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

// shutdownTimeout bounds the whole graceful-shutdown sequence (HTTP drain +
// MQTT offline + media_stop + MQTT disconnect).
const shutdownTimeout = 10 * time.Second

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
	mux.Handle("/stream", stream.New(defaultPreset))

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: mux,
		// No write timeout: the stream handler holds the connection open
		// indefinitely. Read header timeout guards against slow-loris on requests.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// MQTT: discovery entities + command/state handling. Connect wires the
	// on-connect/reconnect reconcile (re-publish discovery, re-assert state).
	mqttSvc, mqttDisconnect, err := mqtt.Connect(ctx, cfg.MQTTBroker, cfg.MQTTUser, cfg.MQTTPass, ctrl)
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
	//  1. Drain/stop the HTTP server so the Sonos stream connection ends.
	//  2. Publish MQTT "offline" so HA shows the entities unavailable promptly.
	//  3. If playback was ON, media_stop the Sonos (needs HA, not MQTT — state is
	//     still read from ctrl before we tear anything down).
	//  4. Disconnect MQTT (paho publishes offline again + closes cleanly).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown: http server", "err", err)
	}

	if err := mqttSvc.PublishOffline(); err != nil {
		slog.Warn("shutdown: publish offline", "err", err)
	}

	if ctrl.IsOn() {
		if err := haClient.MediaStop(shutdownCtx); err != nil {
			slog.Error("shutdown: media_stop", "err", err)
		}
	}

	mqttDisconnect()
	slog.Info("shutdown complete")
}
