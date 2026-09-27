// Command gateway is the zero-trust API gateway binary.
//
// main.go does exactly three things: parse flags, wire dependencies, and
// manage the process lifecycle. All behaviour lives in internal/ packages so
// that it can be unit-tested without starting a real server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ZeynepDilanKilic/sentinel-gateway/internal/config"
	"github.com/ZeynepDilanKilic/sentinel-gateway/internal/proxy"
)

func main() {
	// run() returns an error instead of calling os.Exit directly so that
	// deferred functions (log flush, connection close) actually run.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	// Structured (JSON) logs from day one: a gateway's logs are consumed by
	// machines (Loki, ELK), not humans tailing a file.
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	handler, err := proxy.New(cfg.Routes, proxy.NewTransport(), log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	// Go 1.22 method-aware patterns. /healthz answers "is the process alive";
	// readiness ("can I take traffic") will come later with upstream checks.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", handler)

	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: mux,
		// Each timeout closes a specific attack/failure window:
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration, // slowloris: drip-feeding headers
		ReadTimeout:       cfg.Server.ReadTimeout.Duration,       // slow request bodies
		WriteTimeout:      cfg.Server.WriteTimeout.Duration,      // clients that never read the response
		IdleTimeout:       cfg.Server.IdleTimeout.Duration,       // keep-alive connections parked forever
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// ctx is cancelled on SIGINT/SIGTERM (what Kubernetes/systemd send).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", srv.Addr, "routes", len(cfg.Routes))
		// ErrServerClosed is the *expected* result of Shutdown; not an error.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server failed: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Graceful shutdown: stop accepting, let in-flight requests finish,
	// but never wait forever.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}
