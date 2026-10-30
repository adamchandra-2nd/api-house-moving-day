package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/store"
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

type personBody struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Title string `json:"title"`
	Phone string `json:"phone"`
}

// handleSavePerson adds an MC or speaker (POST) or edits one (PUT {id}).
//
// The cast used to be seeded and nothing else; with SEED=false there was no
// way to put anyone on stage, so nobody could sign in to the MC side at all.
func (s *Server) handleSavePerson(w http.ResponseWriter, r *http.Request) {
	var body personBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	p := model.Person{
		ID:    r.PathValue("id"),
		Name:  strings.TrimSpace(body.Name),
		Role:  body.Role,
		Title: strings.TrimSpace(body.Title),
		Phone: strings.TrimSpace(body.Phone),
	}
	if p.Name == "" {
		s.badRequest(w, "nama wajib diisi")
		return
	}
	if p.Role != "mc" && p.Role != "pembicara" {
		s.badRequest(w, `peran harus "mc" atau "pembicara"`)
		return
	}

	saved, err := s.store.SavePerson(r.Context(), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyPeople)
	status := http.StatusOK
	if p.ID == "" {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, saved)
}

func (s *Server) handleDeletePerson(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePerson(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	// Their sessions were unassigned, so the rundown changed too.
	s.invalidate(r.Context(), keyPeople, keyAgenda)
	s.publishStage(r.Context(), stageEvent{Kind: "rundown"})
	w.WriteHeader(http.StatusNoContent)
}

// handleInvitePerson records that the committee sent or copied someone's
// personal link, so the Pengisi page reads "terkirim" on every laptop.
func (s *Server) handleInvitePerson(w http.ResponseWriter, r *http.Request) {
	if err := s.store.MarkPersonInvited(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyPeople)
	w.WriteHeader(http.StatusNoContent)
}

// handleStageCode tells the committee the code MC and speakers sign in with.
//
// Nothing sends it for them: the committee passes it on by WhatsApp, in a
// message of its own after the link. Until now it lived only in STAGE_CODE,
// so the admin had no way to see what to send.
func (s *Server) handleStageCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, struct {
		Code string `json:"code"`
	}{s.cfg.StageCode})
}

// handleRevokePerson (DELETE) turns a personal link off; POST turns it back
// on. Revoking also ends any session they already hold, on its next request.
func (s *Server) handleRevokePerson(w http.ResponseWriter, r *http.Request) {
	revoked := r.Method == http.MethodDelete
	if err := s.store.SetPersonRevoked(r.Context(), r.PathValue("id"), revoked); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyPeople)
	w.WriteHeader(http.StatusNoContent)
}

// handleStagePerson is what the personal stage link opens with, before any
// sign-in: whose link this is, to greet them by name and say which number the
// code went to. The id is random and the phone is masked, so this says no
// more than the WhatsApp message the link arrived in.
func (s *Server) handleStagePerson(w http.ResponseWriter, r *http.Request) {
	if !s.cache.Allow(r.Context(), "rl:stage-person:"+clientIP(r), loginTriesPerMinute, time.Minute) {
		s.writeJSON(w, http.StatusTooManyRequests, errBody{Error: "terlalu banyak percobaan, tunggu sebentar", Code: "rate_limited"})
		return
	}
	people, err := s.store.People(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	for _, p := range people {
		if p.ID == id {
			if p.Access == "dicabut" {
				s.writeJSON(w, http.StatusGone, errBody{Error: "akses ini sudah dicabut panitia", Code: "revoked"})
				return
			}
			p.Phone = maskPhone(p.Phone)
			s.writeJSON(w, http.StatusOK, p)
			return
		}
	}
	s.fail(w, r, store.ErrNotFound)
}

// maskPhone keeps the first four and last three digits, which is what the
// login screen shows anyway.
func maskPhone(p string) string {
	if len(p) <= 7 {
		return strings.Repeat("•", len(p))
	}
	return p[:4] + strings.Repeat("•", len(p)-7) + p[len(p)-3:]
}

// handleDoor is what the committee prints for the entrance: the URL inside
// the door QR, built from APP_BASE_URL so it opens wherever guests actually
// run the app, and the code printed under it. Scanning it with the phone's own
// camera opens the app on /check-in with the code; the in-app scanner reads
// the same URL.
func (s *Server) handleDoor(w http.ResponseWriter, r *http.Request) {
	doorCode, _ := s.doorCodes(r.Context())
	s.writeJSON(w, http.StatusOK, map[string]string{
		"url":  s.cfg.AppBaseURL + "/check-in?door=" + url.QueryEscape(doorCode),
		"code": doorCode,
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
