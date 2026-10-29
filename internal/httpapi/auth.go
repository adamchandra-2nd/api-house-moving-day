package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/store"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/token"
)

// How long a signed session lasts.
//
// The committee works a full event day on one laptop and the MC holds a phone
// through the whole rundown; being signed out mid-session is worse here than
// a long-lived token, because nobody is going to stop and re-authenticate
// while they are on stage.
const (
	adminSessionTTL = 12 * time.Hour
	stageSessionTTL = 12 * time.Hour
)

// Rate limits. Generous for real use, tight enough that the invite endpoint
// cannot be walked: 80 bits of token times 20 tries a minute is not a route in.
const (
	inviteTriesPerMinute = 20
	loginTriesPerMinute  = 10
)

// inviteAuth resolves a guest from their personal link.
//
// The invite token is the credential, in the same way a wedding RSVP link or a
// shared calendar link is: whoever opens it is treated as that guest. That is
// the right trade for a one-day event with a hundred guests and no accounts,
// and it comes with two obligations that are met here -- the lookup is rate
// limited per IP so tokens cannot be enumerated, and a revoked token is
// refused with 410 rather than quietly behaving like an unknown one.
func (s *Server) inviteAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		// Shape is checked before the database and before the rate limiter, so
		// junk paths cost nothing and do not use up a real guest's budget.
		if !token.ValidInviteShape(tok) {
			s.writeJSON(w, http.StatusUnauthorized, errBody{Error: "buka lewat link undangan kamu", Code: "no_invite"})
			return
		}
		// Only wrong tokens count against the limit. Counting every request
		// throttled real guests: one phone's own polling is about twenty
		// requests a minute, and at the venue every guest on the office wifi
		// shares one public IP. Guessing is what the limit is for, and a
		// guess is a token that does not resolve.
		ip := clientIP(r)
		if _, blocked := s.cache.Get(r.Context(), "rl:invite-block:"+ip); blocked {
			s.writeJSON(w, http.StatusTooManyRequests, errBody{Error: "terlalu banyak percobaan, tunggu sebentar", Code: "rate_limited"})
			return
		}

		guest, err := s.store.GuestByInviteToken(r.Context(), tok)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) && !s.cache.Allow(r.Context(), "rl:invite-miss:"+ip, inviteTriesPerMinute, time.Minute) {
				s.cache.Set(r.Context(), "rl:invite-block:"+ip, []byte("1"), time.Minute)
			}
			s.fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxGuest, guest)))
	})
}

// requireRole guards the admin and stage sides with a signed session.
//
// Unlike the guest side these are shared credentials typed in by a person (the
// committee PIN, the stage code), so they are exchanged for a short-lived
// signed token rather than being sent on every request.
func (s *Server) requireRole(roles ...string) middleware {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, err := token.Verify(s.cfg.SessionSecret, bearer(r))
			if err != nil {
				s.writeJSON(w, http.StatusUnauthorized, errBody{Error: "sesi berakhir, masuk lagi", Code: "no_session"})
				return
			}
			if !allowed[sess.Role] {
				s.writeJSON(w, http.StatusForbidden, errBody{Error: "tidak boleh", Code: "forbidden"})
				return
			}
			// A stage session outlives nothing the committee does: revoking or
			// deleting the person ends it on their next request, not 12 hours
			// later when the token would have expired anyway.
			if sess.Role == "mc" || sess.Role == "pembicara" {
				if code, msg := s.stageStanding(r.Context(), sess.Subject); code != 0 {
					s.writeJSON(w, code, errBody{Error: msg, Code: "revoked"})
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxSession, sess)))
		})
	}
}

// sessionOf returns the verified session for admin and stage handlers.
func sessionOf(r *http.Request) token.Session {
	s, _ := r.Context().Value(ctxSession).(token.Session)
	return s
}

// stageStanding says whether a stage person may still act: 0 when they may,
// 410 when the committee revoked or removed them. One small query per stage
// request: two phones polling is nothing, and a cached answer would let a
// revoked MC keep going for the length of the cache.
func (s *Server) stageStanding(ctx context.Context, personID string) (int, string) {
	people, err := s.store.People(ctx)
	if err != nil {
		// The database being briefly unreachable should not sign the MC out
		// mid-session; the request itself will fail or succeed on its own.
		return 0, ""
	}
	for _, p := range people {
		if p.ID == personID {
			if p.Access == "dicabut" {
				return http.StatusGone, "akses ini sudah dicabut panitia"
			}
			return 0, ""
		}
	}
	return http.StatusGone, "kamu sudah tidak terdaftar sebagai pengisi acara"
}
