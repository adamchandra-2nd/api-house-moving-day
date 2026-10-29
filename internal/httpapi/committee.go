package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
)

// The admin pages that were still editing React state: Tamu, Hadiah, Rundown,
// Pengumuman and Pengaturan.
//
// Each of these is a committee action on data a guest's phone also reads, so
// they all invalidate the caches the guest side serves from. A door code the
// committee changed and a guest's phone has not seen yet is the kind of
// disagreement this file exists to prevent.

// ---- Tamu: manual check-in ----

type adminCheckInBody struct {
	// By is how the guest got in, for the committee's own report. The admin
	// does this on the committee's device, so "panitia" is the default and no
	// PIN is asked for -- being signed in as the committee is the proof.
	By string `json:"by,omitempty"`
}

func (s *Server) handleAdminCheckIn(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body adminCheckInBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	by := body.By
	if by == "" {
		by = "panitia"
	}

	if err := s.store.CheckIn(r.Context(), id, rundownMinute(time.Now()), by); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyGuests, keyStageRoom)
	s.writeJSON(w, http.StatusOK, map[string]any{"guestId": id, "checkedIn": true, "by": by})
}

// handleAdminUndoCheckIn is the committee taking back a mistake at the door.
func (s *Server) handleAdminUndoCheckIn(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.UndoCheckIn(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyGuests, keyStageRoom)
	s.writeJSON(w, http.StatusOK, map[string]any{"guestId": id, "checkedIn": false})
}

// ---- Hadiah ----

// handleClaimReward hands out a prize, once.
//
// The store refuses a second claim rather than this handler checking first:
// two committee phones on the same guest is the case that matters, and a
// read-then-write here would let both of them pass the check before either
// wrote. A refusal comes back as 409.
func (s *Server) handleClaimReward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	minute := rundownMinute(time.Now())

	if err := s.store.ClaimReward(r.Context(), id, minute); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyStageRoom)
	s.writeJSON(w, http.StatusOK, map[string]any{"guestId": id, "reward": model.RewardClaimed, "claimedAt": minute})
}

// ---- Rundown, Pengumuman, Pengisi: the committee's read side ----

// The guest-facing endpoints for these are cached per guest token and gated by
// one, so the committee cannot use them. These are the same data under the
// admin session, sharing the same cache entries.

func (s *Server) handleAdminRundown(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyAgenda, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		return s.store.Sessions(ctx)
	})
}

func (s *Server) handleAdminAnnouncements(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyAnnouncements, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		return s.store.Announcements(ctx)
	})
}

func (s *Server) handleAdminPeople(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyPeople, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		return s.store.People(ctx)
	})
}

// ---- Pengaturan ----

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keySettings, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		return s.store.Settings(ctx)
	})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body model.Settings
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if msg, ok := validSettings(&body); !ok {
		s.badRequest(w, msg)
		return
	}

	if err := s.store.SaveSettings(r.Context(), body); err != nil {
		s.fail(w, r, err)
		return
	}
	// keyEvent is built from this row too, so it goes with it. Without
	// this, a countdown the committee just switched off keeps showing on
	// guests' phones until the cache expires on its own.
	s.invalidate(r.Context(), keySettings, keyEvent)
	s.writeJSON(w, http.StatusOK, body)
}

// validSettings trims and checks the row before it becomes the thing the door
// compares against.
//
// The codes are the reason this is strict. An empty door code would be
// accepted by every guest who submits an empty field, and a code with a space
// in it is a code nobody can type correctly from a sign.
func validSettings(s *model.Settings) (string, bool) {
	s.EventName = strings.TrimSpace(s.EventName)
	s.Place.Name = strings.TrimSpace(s.Place.Name)
	s.Place.Address = strings.TrimSpace(s.Place.Address)
	s.DoorCode = strings.TrimSpace(s.DoorCode)
	s.PanitiaPIN = strings.TrimSpace(s.PanitiaPIN)

	switch {
	case s.EventName == "":
		return "nama acara wajib diisi", false
	case !digits(s.DoorCode):
		return "kode pintu harus 4-8 angka", false
	case !digits(s.PanitiaPIN):
		return "PIN panitia harus 4-8 angka", false
	case !isDay(s.Date):
		return "tanggal harus format YYYY-MM-DD", false
	case s.FinalFeedbackAt < 0 || s.FinalFeedbackAt > 24*60:
		return "jam feedback di luar rentang", false
	case s.CodesChangedAt < 0 || s.CodesChangedAt > 24*60:
		return "jam ganti kode di luar rentang", false
	}
	return "", true
}

func digits(s string) bool {
	if len(s) < 4 || len(s) > 8 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isDay(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// doorCodes reads the two codes the check-in endpoint compares against.
//
// They come from the settings row, not the environment: the committee can
// change them on the Pengaturan page during the event, and a code that only
// lives in a deploy's environment could not be rotated without a restart.
// The environment values remain as the row's initial contents.
//
// A read failure is not fatal -- it falls back to the configured codes --
// because a door that cannot check anyone in is worse than a door using the
// code it booted with.
func (s *Server) doorCodes(ctx context.Context) (door, panitia string) {
	set, err := s.store.Settings(ctx)
	if err != nil || set.DoorCode == "" {
		return s.cfg.DoorCode, s.cfg.PanitiaPIN
	}
	return set.DoorCode, set.PanitiaPIN
}
