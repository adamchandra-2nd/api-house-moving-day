// Package app wires a configured API: cache, store and routes.
//
// The long-running server (cmd/server) and the Vercel function (api/index.go)
// both start here, so the two can only differ in how requests reach them,
// never in which database, cache or routes answer.
package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/cache"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/config"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/httpapi"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/store"
)

// App is a ready API. Close releases the database pool and the cache.
type App struct {
	Handler http.Handler
	Store   store.Store
	Cache   cache.Cache
}

func (a *App) Close() {
	a.Store.Close()
	a.Cache.Close()
}

// Open connects the cache and the store and builds the routes. ctx bounds the
// connecting only, not the App's lifetime.
func Open(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	ch, err := cache.Open(ctx, cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	st, err := openStore(ctx, cfg, log)
	if err != nil {
		ch.Close()
		return nil, err
	}
	return &App{Handler: httpapi.New(cfg, st, ch, log).Routes(), Store: st, Cache: ch}, nil
}

// openStore picks the backend and, on a brand new Supabase project, loads the
// sample data so the API behaves the same way it does in memory.
func openStore(ctx context.Context, cfg *config.Config, log *slog.Logger) (store.Store, error) {
	if cfg.InMemory() {
		return store.NewMemory(cfg.Seed)
	}

	pg, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if !cfg.Seed {
		log.Info("SEED=false: leaving the database as it is, sample data included if it is already there")
		return pg, nil
	}
	seeded, err := pg.SeedIfEmpty(ctx)
	if err != nil {
		pg.Close()
		// A seed that half-applied would leave a confusing database, so the
		// transaction rolls back and this is fatal rather than a warning.
		return nil, err
	}
	if seeded {
		log.Info("seeded an empty database with the sample guests, rundown and questions")
	}
	return pg, nil
}
