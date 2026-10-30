package httpapi

import (
	"net/http"
)

// Routes returns the whole API.
//
// Every route is registered on one mux so r.PathValue keeps working, with its
// own auth wrapper rather than a nested mux -- which makes the access rule for
// each endpoint visible on the line that declares it, instead of hidden in how
// the muxes are mounted.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// guest: the caller holds an invite link.
	guest := func(fn http.HandlerFunc) http.Handler { return s.inviteAuth(fn) }
	// admin: the committee, signed in with email + PIN.
	admin := func(fn http.HandlerFunc) http.Handler { return s.requireRole("admin")(fn) }
	// stage: MC or speaker, signed in with the stage code.
	stage := func(fn http.HandlerFunc) http.Handler { return s.requireRole("mc", "pembicara")(fn) }
	// live: both stage sides plus the committee bell share one stream.
	live := func(fn http.HandlerFunc) http.Handler { return s.requireRole("mc", "pembicara", "admin")(fn) }

	// ---- open ----

	mux.HandleFunc("GET /healthz", s.handleHealth)

	// The event's name, date, place and the countdown switch. No credential:
	// it is what is written on the invitation, and the app's first screen
	// needs it before a guest has necessarily opened their personal link.
	mux.HandleFunc("GET /api/event", s.handleEvent)

	// ---- the guest's own invite link ----
	//
	// The token travels as a bearer credential, never in the path: paths are
	// logged and this one unlocks a guest's identity.

	mux.Handle("GET /api/invite", guest(s.handleInvite))
	mux.Handle("POST /api/invite/rsvp", guest(s.handleRsvp))
	mux.Handle("POST /api/invite/checkin", guest(s.handleCheckIn))
	mux.Handle("POST /api/invite/hadir", guest(s.handleHadir))
	// The voucher screen: the committee types their PIN on the guest's phone.
	mux.Handle("POST /api/invite/reward", guest(s.handleClaimOwnReward))

	// ---- guest reads ----

	mux.Handle("GET /api/guests", guest(s.handleGuests))
	mux.Handle("GET /api/guests/{id}/sprite", guest(s.handleSprite))
	mux.Handle("PUT /api/invite/sprite", guest(s.handleSetSprite))
	mux.Handle("GET /api/kenalan/invites", guest(s.handleKenalanInvites))
	mux.Handle("POST /api/kenalan/invites", guest(s.handleKenalanInvite))
	mux.Handle("GET /api/agenda", guest(s.handleAgenda))
	mux.Handle("GET /api/live", guest(s.handleLive))
	mux.Handle("GET /api/announcements", guest(s.handleAnnouncements))
	mux.Handle("GET /api/people", guest(s.handlePeople))
	mux.Handle("GET /api/questions", guest(s.handleQuestions))
	mux.Handle("GET /api/cards", guest(s.handleMyCards))

	// ---- guest writes ----

	mux.Handle("POST /api/questions", guest(s.handleSendQuestion))
	mux.Handle("PATCH /api/questions/{id}", guest(s.handleEditQuestion))
	mux.Handle("DELETE /api/questions/{id}", guest(s.handleDeleteQuestion))
	mux.Handle("POST /api/cards", guest(s.handleCollectCard))
	mux.Handle("POST /api/feedback/event", guest(s.handleEventFeedback))
	mux.Handle("POST /api/feedback/final", guest(s.handleFinalFeedback))

	// The shared gallery. The image bytes are a separate request per photo so
	// each one caches on its own; the list stays small JSON.
	mux.Handle("GET /api/photos", guest(s.handlePhotos))
	mux.Handle("POST /api/photos", guest(s.handleAddPhoto))
	mux.Handle("DELETE /api/photos/{id}", guest(s.handleDeletePhoto))
	mux.Handle("GET /api/photos/{id}/image", guest(s.handlePhotoImage))

	// ---- committee ----

	mux.HandleFunc("POST /api/admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /api/admin/verify", s.handleAdminVerify)

	mux.Handle("GET /api/admin/guests", admin(s.handleAdminGuests))
	mux.Handle("POST /api/admin/guests", admin(s.handleAddGuest))
	mux.Handle("POST /api/admin/invites", admin(s.handleSendInvites))
	mux.Handle("DELETE /api/admin/guests/{id}/invite", admin(s.handleRevokeInvite))
	mux.Handle("GET /api/admin/dashboard", admin(s.handleDashboard))
	mux.Handle("GET /api/admin/feedback", admin(s.handleAdminFeedback))

	// Tamu and Hadiah: what the committee does to a guest's row from their
	// own device, rather than from the guest's phone.
	mux.Handle("POST /api/admin/guests/{id}/checkin", admin(s.handleAdminCheckIn))
	mux.Handle("DELETE /api/admin/guests/{id}/checkin", admin(s.handleAdminUndoCheckIn))
	mux.Handle("POST /api/admin/guests/{id}/reward", admin(s.handleClaimReward))

	// The same data the guest side reads, under the committee's session: the
	// guest endpoints need an invite token, which the committee has not got.
	mux.Handle("GET /api/admin/rundown", admin(s.handleAdminRundown))
	mux.Handle("PUT /api/admin/rundown", admin(s.handleSaveRundown))
	mux.Handle("GET /api/admin/announcements", admin(s.handleAdminAnnouncements))
	mux.Handle("POST /api/admin/announcements", admin(s.handleAddAnnouncement))
	mux.Handle("POST /api/admin/announcements/{id}/resend", admin(s.handleResendAnnouncement))
	mux.Handle("DELETE /api/admin/announcements/{id}", admin(s.handleDeleteAnnouncement))
	mux.Handle("GET /api/admin/people", admin(s.handleAdminPeople))
	mux.Handle("POST /api/admin/people", admin(s.handleSavePerson))
	mux.Handle("PUT /api/admin/people/{id}", admin(s.handleSavePerson))
	mux.Handle("DELETE /api/admin/people/{id}", admin(s.handleDeletePerson))
	mux.Handle("POST /api/admin/people/{id}/invite", admin(s.handleInvitePerson))
	mux.Handle("DELETE /api/admin/people/{id}/access", admin(s.handleRevokePerson))
	mux.Handle("POST /api/admin/people/{id}/access", admin(s.handleRevokePerson))

	// Galeri: the same photos with the committee's own notes, and the two
	// actions only they can take.
	mux.Handle("GET /api/admin/photos", admin(s.handleAdminPhotos))
	mux.Handle("GET /api/admin/photos/{id}/image", admin(s.handlePhotoImage))
	mux.Handle("POST /api/admin/photos/censor", admin(s.handleCensorPhotos))
	mux.Handle("POST /api/admin/photos/checked", admin(s.handlePhotosChecked))

	mux.Handle("GET /api/admin/settings", admin(s.handleSettings))
	mux.Handle("GET /api/admin/door", admin(s.handleDoor))
	mux.Handle("PUT /api/admin/settings", admin(s.handleSaveSettings))

	// ---- MC and Performance ----

	mux.HandleFunc("POST /api/stage/login", s.handleStageLogin)
	mux.HandleFunc("GET /api/stage/person/{id}", s.handleStagePerson)
	mux.Handle("GET /api/stage/people", stage(s.handlePeople))
	// What the MC and speaker screens start from, instead of a bundled copy.
	mux.Handle("GET /api/stage/rundown", live(s.handleStageRundown))
	mux.Handle("GET /api/stage/questions", stage(s.handleStageQuestions))
	mux.Handle("GET /api/stage/room", stage(s.handleStageRoom))
	mux.Handle("GET /api/stage/state", live(s.handleStageState))
	// The committee writes here too: the scripts on the Pengisi page live in
	// this document, and the MC reads them from it. Left to the two stage
	// roles, a script the committee wrote would never leave their laptop.
	mux.Handle("PUT /api/stage/state", live(s.handleSaveStageState))
	mux.Handle("PATCH /api/questions/{id}/status", stage(s.handleQuestionStatus))
	// The MC's Umumkan tab, reaching guests' phones like the committee's.
	mux.Handle("POST /api/stage/announcements", s.requireRole("mc")(http.HandlerFunc(s.handleStageAnnouncement)))

	// The live stream: MC, speaker and the committee's bell.
	mux.Handle("GET /api/stage/stream", live(s.handleStageStream))

	return chain(mux, s.recoverPanic, s.logging, s.cors)
}

type health struct {
	OK    bool   `json:"ok"`
	Store string `json:"store"`
	Cache string `json:"cache"`
	// Mode says plainly whether anything is being persisted, because
	// "everything works" and "everything works and will be gone on restart"
	// look identical from the outside.
	Mode string `json:"mode"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	mode := "persistent"
	if s.cfg.InMemory() {
		mode = "ephemeral (no DATABASE_URL: data resets on restart)"
	}
	s.writeJSON(w, http.StatusOK, health{
		OK:    true,
		Store: s.store.Kind(),
		Cache: s.cache.Kind(),
		Mode:  mode,
	})
}
