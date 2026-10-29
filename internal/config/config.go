// Package config reads every knob from the environment, so the same binary
// runs locally with nothing configured and in production with Supabase and
// Redis behind it.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Addr string

	// DatabaseURL is the Supabase Postgres connection string. Empty means the
	// in-memory store: the API runs with the same sample data the frontend
	// ships with, so the whole workflow is testable before signing up for
	// anything. Nothing is persisted across restarts in that mode.
	DatabaseURL string

	// RedisURL is the Upstash (or local) Redis URL. Empty means an in-process
	// stand-in that implements the same interface, so caching, idempotency,
	// rate limiting and stage fan-out all still work on one instance.
	RedisURL string

	// AppBaseURL is the public origin of the React app. Invite links are built
	// from it, so it must match where guests actually open the app.
	AppBaseURL string

	// AllowedOrigins is the CORS allowlist for the browser app.
	AllowedOrigins []string

	// SessionSecret signs admin and stage session tokens. A random one is
	// generated at boot if unset, which logs everybody out on restart.
	SessionSecret string

	// AdminEmail and AdminPIN are the prototype committee credentials, kept
	// identical to src/admin/data.ts so the existing login screen works.
	AdminEmail string
	AdminPIN   string

	// StageCode is what MC and speakers type in, from src/stage/StageLogin.tsx.
	StageCode string

	// DoorCode and PanitiaPIN mirror src/config.ts.
	DoorCode   string
	PanitiaPIN string

	// CacheTTL is how long Redis holds a cached read. Short enough that an
	// admin edit shows up quickly, long enough to absorb a room full of
	// phones refreshing at once.
	CacheTTL time.Duration

	// Seed fills an empty database with the sample guests, rundown and
	// questions. On by default, because an API that answers with nothing is
	// hard to tell apart from one that is broken.
	//
	// Turn it off (SEED=false) to run the real event: the committee then
	// enters its own guest list and every number on the dashboard is real
	// from the first one.
	Seed bool
}

// Load reads .env (if present) and then the real environment, which wins.
func Load() (*Config, error) {
	loadDotEnv(".env")

	c := &Config{
		Addr:           listenAddr(),
		DatabaseURL:    env("DATABASE_URL", ""),
		RedisURL:       env("REDIS_URL", ""),
		AppBaseURL:     strings.TrimRight(env("APP_BASE_URL", "http://localhost:5173"), "/"),
		AllowedOrigins: split(env("ALLOWED_ORIGINS", "http://localhost:5173,http://localhost:4173")),
		SessionSecret:  env("SESSION_SECRET", ""),
		AdminEmail:     env("ADMIN_EMAIL", "admin@potara.id"),
		AdminPIN:       env("ADMIN_PIN", "123456"),
		StageCode:      env("STAGE_CODE", "246810"),
		DoorCode:       env("DOOR_CODE", "1108"),
		PanitiaPIN:     env("PANITIA_PIN", "2026"),
		CacheTTL:       15 * time.Second,
		Seed:           env("SEED", "true") != "false",
	}

	if c.AppBaseURL == "" {
		return nil, fmt.Errorf("APP_BASE_URL must not be empty: invite links are built from it")
	}
	return c, nil
}

// InMemory reports whether this process is running without Postgres.
func (c *Config) InMemory() bool { return c.DatabaseURL == "" }

// listenAddr decides what to bind to.
//
// ADDR wins, because it is the one that can say *which interface* as well as
// which port. Railway, Render and Heroku instead inject PORT and route traffic
// to whatever that says; binding 8080 there means the health check never gets
// an answer and the deploy is rolled back. So PORT is honoured next, and the
// host is left empty -- "all interfaces" -- because the request arrives from
// the platform's proxy, not from localhost.
func listenAddr() string {
	if a := env("ADDR", ""); a != "" {
		return a
	}
	if p := env("PORT", ""); p != "" {
		return ":" + p
	}
	return ":8080"
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func split(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadDotEnv is a deliberately small .env reader: KEY=value, # comments, and
// optional surrounding quotes. It never overwrites a variable that is already
// set, so the real environment always wins over the file.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}
