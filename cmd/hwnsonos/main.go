// Command hwnsonos synthesizes continuous brown/pink/white noise and streams it
// to a Sonos speaker via Home Assistant. This is the Phase 0 scaffold: it loads
// config and serves /healthz. The noise stream, HA orchestration, and MQTT
// control entities are wired in later phases.
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
		// No write timeout: the stream handler (added later) holds the connection
		// open indefinitely. Read timeout guards against slow-loris on requests.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down cleanly on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("hwnsonos listening on %s (preset=%s volume=%d target=%s)",
			cfg.HTTPAddr, cfg.DefaultPreset, cfg.DefaultVolume, cfg.HAMediaPlayer)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
		os.Exit(1)
	}
}
