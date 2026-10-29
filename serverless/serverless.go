// Package serverless is the API behind a Vercel Go function.
//
// It lives outside internal/ on purpose: Vercel compiles api/index.go under
// its own package path, and Go only lets code inside this module import
// internal/. api/index.go calls Handler here; this package imports the rest.
//
// Vercel calls Handler for every request; vercel.json rewrites every path to
// it, so the router sees the same paths it does under cmd/server. The app is
// built on the first request an instance gets and reused for the rest of
// that instance's life, which is what keeps the database pool warm.
package serverless

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/app"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/config"
)

var (
	mu    sync.Mutex
	built http.Handler
	log   = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
)

func Handler(w http.ResponseWriter, r *http.Request) {
	h, err := handler()
	if err != nil {
		log.Error("boot", "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"server belum siap","code":"boot_failed"}`))
		return
	}
	h.ServeHTTP(w, r)
}

// handler builds the app once per instance. A failed build is not cached, so
// a database that was briefly unreachable is retried on the next request
// instead of leaving the instance broken until Vercel recycles it.
func handler() (http.Handler, error) {
	mu.Lock()
	defer mu.Unlock()
	if built != nil {
		return built, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	// Each instance would otherwise invent its own secret and refuse every
	// other instance's sessions, and an in-memory store would be a different
	// guest list per instance. Both are fatal here, not warnings.
	if cfg.SessionSecret == "" {
		return nil, errors.New("SESSION_SECRET must be set on Vercel")
	}
	if cfg.InMemory() {
		return nil, errors.New("DATABASE_URL must be set on Vercel")
	}
	cfg.Serverless = true
	// Each instance has its own in-memory cache, and a write clears only the
	// instance that took it. A short TTL bounds how long another instance can
	// still serve the old rundown or announcements to guests.
	cfg.CacheTTL = 3 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a, err := app.Open(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	built = a.Handler
	return built, nil
}
