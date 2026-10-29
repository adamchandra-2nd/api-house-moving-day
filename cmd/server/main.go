// Command server runs the HMD Event Companion API.
//
// It starts with nothing configured: no DATABASE_URL means an in-memory store
// seeded with the same sample data the React app ships with, and no REDIS_URL
// means an in-process stand-in. So the whole workflow -- personal invite
// links, RSVP, check-in, questions, the live MC/speaker sync -- is testable
// before signing up for any service, and adding the two URLs later changes no
// code.
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

	"github.com/adamchandra-2nd/api-house-moving-day/internal/app"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/config"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/token"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.SessionSecret == "" {
		cfg.SessionSecret = token.NewSecret()
		log.Warn("SESSION_SECRET not set: generated a temporary one, so every restart signs the committee and the MC out")
	}

	// Boot gets its own deadline. Hanging forever on an unreachable database
	// is the worst failure mode here: it looks like a slow start right up
	// until the event begins.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := app.Open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: a.Handler,

		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: the stage stream is a long-lived response and a
		// write deadline would cut it off mid-event. Idle connections are
		// bounded by IdleTimeout and the stream's own heartbeat instead.
		IdleTimeout: 2 * time.Minute,
	}

	log.Info("listening",
		"addr", cfg.Addr,
		"store", a.Store.Kind(),
		"cache", a.Cache.Kind(),
		"app", cfg.AppBaseURL,
		"invite_links", cfg.AppBaseURL+"/u/<token>")
	if cfg.InMemory() {
		log.Warn("no DATABASE_URL: running in memory, every guest gets a fresh invite token on restart")
	}

	// Shut down on Ctrl-C without dropping requests mid-flight.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errs := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-stop:
		log.Info("shutting down")
		// Long enough to finish real requests, short enough that the stage
		// streams -- which never end on their own -- do not hold it open.
		down, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(down)
	}
}
