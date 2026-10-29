// Package httpapi is the HTTP surface: routing, auth, and the response
// plumbing that makes the API cheap to call from a phone on venue wifi.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/cache"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/config"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/store"
)

// Server holds everything the handlers need. One value, passed by pointer, so
// there are no package-level globals to make tests awkward.
type Server struct {
	cfg   *config.Config
	store store.Store
	cache cache.Cache
	log   *slog.Logger
}

func New(cfg *config.Config, st store.Store, ch cache.Cache, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: st, cache: ch, log: log}
}

// ---------- context ----------

type ctxKey int

const (
	ctxGuest ctxKey = iota
	ctxSession
)

// guestOf returns the guest resolved by inviteAuth. Handlers behind that
// middleware can rely on it being present.
func guestOf(r *http.Request) model.AdminGuest {
	g, _ := r.Context().Value(ctxGuest).(model.AdminGuest)
	return g
}

// ---------- responses ----------

type errBody struct {
	Error string `json:"error"`
	// Code is a stable machine-readable reason, so the frontend can branch on
	// it without parsing prose: "revoked", "forbidden", "conflict", ...
	Code string `json:"code,omitempty"`
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Error("marshal response", "err", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

// fail maps a store error onto the right status code, in one place, so no
// handler has to remember that a revoked invite is 410 and not 404.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, errBody{Error: "tidak ditemukan", Code: "not_found"})
	case errors.Is(err, store.ErrRevoked):
		// 410 Gone, not 404: the link was real, the committee turned it off.
		s.writeJSON(w, http.StatusGone, errBody{Error: "undangan ini sudah tidak berlaku", Code: "revoked"})
	case errors.Is(err, store.ErrForbidden):
		s.writeJSON(w, http.StatusForbidden, errBody{Error: "tidak boleh", Code: "forbidden"})
	case errors.Is(err, store.ErrConflict):
		// The store writes these for a person to read ("hadiah sudah
		// diambil"), but wraps them in the sentinel, so the prefix is dropped
		// rather than shown to the committee as "conflict: hadiah sudah...".
		s.writeJSON(w, http.StatusConflict, errBody{Error: unwrapMessage(err, store.ErrConflict), Code: "conflict"})
	case errors.Is(err, store.ErrDuplicate):
		s.writeJSON(w, http.StatusConflict, errBody{Error: "sudah ada di daftar", Code: "duplicate"})
	case errors.Is(err, cache.ErrInFlight):
		// The same offline-queued write is already being processed. 409 with a
		// Retry-After, so the client's replay backs off instead of hammering.
		w.Header().Set("Retry-After", "1")
		s.writeJSON(w, http.StatusConflict, errBody{Error: "permintaan yang sama sedang diproses", Code: "in_flight"})
	case errors.Is(err, context.Canceled):
		// The phone walked out of wifi range mid-request. Nothing to report.
	default:
		// Log the real error, return a short one: a Postgres message can carry
		// column names and connection details.
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		s.writeJSON(w, http.StatusInternalServerError, errBody{Error: "ada kesalahan di server", Code: "internal"})
	}
}

// unwrapMessage returns the part of err that is not the sentinel it wraps.
//
// The store writes `fmt.Errorf("%w: hadiah sudah diambil", ErrConflict)`,
// which is right for a log and wrong for a screen. Falls back to a plain
// message when there is nothing beyond the sentinel itself.
func unwrapMessage(err, sentinel error) string {
	msg := strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
	if msg == "" || msg == err.Error() {
		return "tidak bisa diproses sekarang"
	}
	return msg
}

func (s *Server) badRequest(w http.ResponseWriter, msg string) {
	s.writeJSON(w, http.StatusBadRequest, errBody{Error: msg, Code: "bad_request"})
}

// decode reads a JSON body with a size limit, because an unbounded read is how
// one bad client takes the process down.
func decode[T any](w http.ResponseWriter, r *http.Request, dst *T) error {
	const maxBody = 1 << 20 // 1 MiB: far more than any payload here
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("body tidak valid: %w", err)
	}
	return nil
}

// ---------- cached reads ----------

// cachedJSON serves a read through Redis and an ETag.
//
// Both layers matter on venue wifi: Redis means a room full of phones costs
// one database query rather than one each, and the ETag means a phone that
// already has the current version gets a 42-byte 304 instead of the payload.
func (s *Server) cachedJSON(w http.ResponseWriter, r *http.Request, key string, ttl time.Duration, load func(context.Context) (any, error)) {
	body, hit := s.cache.Get(r.Context(), key)
	if !hit {
		v, err := load(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		body = b
		s.cache.Set(r.Context(), key, body, ttl)
	}

	sum := sha256.Sum256(body)
	etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	// must-revalidate: a stale rundown is worse than a round trip, and the
	// round trip is a 304 anyway.
	w.Header().Set("Cache-Control", "private, max-age=5, must-revalidate")

	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(body)
}

// invalidate drops cache keys after a write. Called with the keys a write
// could possibly affect; over-invalidating costs one query, under-invalidating
// shows the committee stale numbers.
func (s *Server) invalidate(ctx context.Context, keys ...string) {
	s.cache.Delete(ctx, keys...)
}

// Cache keys. Collected here so a write can see at a glance what it affects.
const (
	keyGuests        = "read:guests"
	keyAgenda        = "read:agenda"
	keyAnnouncements = "read:announcements"
	keyPeople        = "read:people"
	keyQuestions     = "read:questions"
	keyAdminGuests   = "read:admin:guests"
	keyDashboard     = "read:admin:dashboard"
	keySettings      = "read:settings"
	keyEvent         = "read:event"
	keyStageRoom     = "read:stage:room"
	keyPhotos        = "read:photos"
	keyFeedback      = "read:admin:feedback"
)

// ---------- idempotent writes ----------

// idempotent runs a write once per Idempotency-Key and replays the stored
// response for repeats.
//
// This is what makes the frontend's offline queue safe. react-query pauses
// mutations while the phone is offline, persists them to IndexedDB and replays
// them when the signal returns -- and a replay can also happen because the
// response was lost, not because the write failed. Without this, "send
// question" twice means two questions.
//
// A request with no key runs normally: the client decides which writes are
// worth protecting, and every write that the offline queue can replay sends
// one.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, scope string, produce func() (any, error)) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		v, err := produce()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.writeJSON(w, http.StatusOK, v)
		return
	}
	if len(key) > 200 {
		s.badRequest(w, "Idempotency-Key terlalu panjang")
		return
	}

	// Scoped per caller: two guests may legitimately generate the same uuid,
	// and one must never receive the other's response.
	full := "idem:" + scope + ":" + key

	body, replayed, err := s.cache.Once(r.Context(), full, 24*time.Hour, func() ([]byte, error) {
		v, err := produce()
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	if replayed {
		// Tells the client this was a duplicate it can stop retrying, without
		// changing the body it would have got the first time.
		w.Header().Set("Idempotent-Replay", "true")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(body)
}

// ---------- middleware ----------

type middleware func(http.Handler) http.Handler

func chain(h http.Handler, ms ...middleware) http.Handler {
	// Applied in reverse so the first listed middleware is the outermost.
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// cors answers preflights and sets the headers the browser app needs.
//
// An allowlist, never "*": these responses carry a guest's own data and the
// committee's guest list, and the Idempotency-Key header has to be explicitly
// allowed or the browser strips it.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range s.cfg.AllowedOrigins {
		allowed[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, If-None-Match")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			// Without this the app cannot read the replay marker or the ETag.
			w.Header().Set("Access-Control-Expose-Headers", "ETag, Idempotent-Replay, Retry-After")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logging records one line per request. Paths are logged, query strings are
// not: an invite token can arrive as ?t= and must not end up in the logs.
func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		s.log.Log(r.Context(), level, "http",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Flush keeps the SSE handler working through this wrapper: without it the
// recorder hides the underlying Flusher and nothing reaches the client until
// the stream ends.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recoverPanic keeps one bad request from taking the event down mid-session.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "path", r.URL.Path, "value", v)
				s.writeJSON(w, http.StatusInternalServerError, errBody{Error: "ada kesalahan di server", Code: "internal"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// bearer pulls the credential out of a request.
//
// The header is the normal path. The query parameter exists because the very
// first thing a guest's phone loads is their link, and an <img> or a service
// worker prefetch cannot set headers -- but it is accepted only where the
// route expects it, and never logged.
func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(r.URL.Query().Get("t"))
}

// clientIP is best effort, for rate limiting only. It is never trusted for
// authorisation, so a forged header buys nothing but a shared bucket.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		if host, _, ok := strings.Cut(f, ","); ok {
			return strings.TrimSpace(host)
		}
		return strings.TrimSpace(f)
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}
