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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kevin/ha-white-noise-sonos/internal/config"
	"github.com/kevin/ha-white-noise-sonos/internal/control"
	"github.com/kevin/ha-white-noise-sonos/internal/ha"
	"github.com/kevin/ha-white-noise-sonos/internal/mqtt"
	"github.com/kevin/ha-white-noise-sonos/internal/noise"
	"github.com/kevin/ha-white-noise-sonos/internal/stream"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("startup: %v", err)
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
		log.Fatalf("mqtt: %v", err)
	}
	defer mqttDisconnect()
	_ = mqttSvc // state is published via the on-connect handler and command flow.

	// Watchdog: re-play if the Sonos drops the stream while the switch is ON.
	go watchdog.Run(ctx)

	go func() {
		log.Printf("hwnsonos listening on %s (preset=%s volume=%d target=%s)",
			cfg.HTTPAddr, cfg.DefaultPreset, cfg.DefaultVolume, cfg.HAMediaPlayer)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	stop() // restore default signal handling; a second signal now force-quits.

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// If playback is on, stop it on the Sonos before exiting.
	if ctrl.IsOn() {
		if err := haClient.MediaStop(shutdownCtx); err != nil {
			log.Printf("shutdown: media_stop: %v", err)
		}
	}

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
		os.Exit(1)
	}
}
