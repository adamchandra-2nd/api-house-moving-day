package httpapi

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/token"
)

// The committee side.

// waTemplate mirrors DEFAULT_TEMPLATE in src/admin/invite.ts, so the message
// the API hands back is the one the admin screen already previews.
const waTemplate = "Halo {nama}! \U0001F44B\n\n" +
	"Potara pindah rumah dan kami ingin merayakannya bareng kamu di House Moving Day, Minggu 8 November 2026.\n\n" +
	"Konfirmasi kehadiranmu di sini:\n{link}\n\n" +
	"Sampai ketemu di rumah baru!"

type adminLoginBody struct {
	Email string `json:"email"`
}

// adminLoginResult never carries the PIN. There is no mail service, so the
// committee gets ADMIN_PIN from whoever runs the server; returning it here
// would let anyone who knows the address sign in.
type adminLoginResult struct {
	Sent bool `json:"sent"`
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cache.Allow(r.Context(), "rl:admin-login:"+clientIP(r), loginTriesPerMinute, time.Minute) {
		s.writeJSON(w, http.StatusTooManyRequests, errBody{Error: "terlalu banyak percobaan, tunggu sebentar", Code: "rate_limited"})
		return
	}

	var body adminLoginBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}

	// The same answer either way: whether an address is a committee address is
	// not something an unauthenticated caller should be able to probe.
	if !strings.EqualFold(strings.TrimSpace(body.Email), s.cfg.AdminEmail) {
		s.writeJSON(w, http.StatusOK, adminLoginResult{Sent: true})
		return
	}
	s.log.Info("admin PIN requested", "email", body.Email)
	s.writeJSON(w, http.StatusOK, adminLoginResult{Sent: true})
}

type adminVerifyBody struct {
	Email string `json:"email"`
	PIN   string `json:"pin"`
}

type adminVerifyResult struct {
	Token string `json:"token"`
	Email string `json:"email"`
}

func (s *Server) handleAdminVerify(w http.ResponseWriter, r *http.Request) {
	if !s.cache.Allow(r.Context(), "rl:admin-verify:"+clientIP(r), loginTriesPerMinute, time.Minute) {
		s.writeJSON(w, http.StatusTooManyRequests, errBody{Error: "terlalu banyak percobaan, tunggu sebentar", Code: "rate_limited"})
		return
	}

	var body adminVerifyBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}

	emailOK := strings.EqualFold(strings.TrimSpace(body.Email), s.cfg.AdminEmail)
	pinOK := subtle.ConstantTimeCompare([]byte(body.PIN), []byte(s.cfg.AdminPIN)) == 1
	if !emailOK || !pinOK {
		s.writeJSON(w, http.StatusForbidden, errBody{Error: "email atau PIN salah", Code: "bad_pin"})
		return
	}

	sess := token.Session{Subject: s.cfg.AdminEmail, Role: "admin", Expires: time.Now().Add(adminSessionTTL)}
	s.writeJSON(w, http.StatusOK, adminVerifyResult{Token: token.Sign(s.cfg.SessionSecret, sess), Email: s.cfg.AdminEmail})
}

// handleAdminGuests is the committee's guest list, invite links included.
func (s *Server) handleAdminGuests(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyAdminGuests, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		guests, err := s.store.AdminGuests(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]model.AdminGuest, 0, len(guests))
		for _, g := range guests {
			g.InviteURL = s.inviteURL(g.InviteToken, g.Name)
			out = append(out, g)
		}
		return out, nil
	})
}

type addGuestBody struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
	Email string `json:"email,omitempty"`
}

// handleAddGuest adds one guest and mints their personal link.
//
// The token is generated here, server-side. In the prototype the link is
// derived from the guest's name (inviteLink in src/admin/invite.ts), which
// means anybody can guess anybody else's link by typing their name -- fine for
// a mockup, not for a list of real phone numbers.
func (s *Server) handleAddGuest(w http.ResponseWriter, r *http.Request) {
	var body addGuestBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		s.badRequest(w, "nama wajib diisi")
		return
	}
	phone := normalizePhone(body.Phone)
	if phone == "" {
		s.badRequest(w, "nomor WhatsApp tidak valid (contoh: 081234567890)")
		return
	}

	tok, err := token.NewInvite()
	if err != nil {
		s.fail(w, r, err)
		return
	}

	guest, err := s.store.AddGuest(r.Context(), body.Name, phone, strings.TrimSpace(body.Email), tok)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyStageRoom)

	guest.InviteURL = s.inviteURL(guest.InviteToken, guest.Name)
	s.writeJSON(w, http.StatusCreated, guest)
}

type sendInvitesBody struct {
	GuestIDs []string `json:"guestIds"`
}

type invitePlan struct {
	GuestID string `json:"guestId"`
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	URL     string `json:"url"`
	// Message is the filled-in template, and WaURL opens WhatsApp with it
	// already typed. This is what makes the flow testable today: no WhatsApp
	// Business account needed, the committee just taps each link.
	Message string `json:"message"`
	WaURL   string `json:"waUrl"`
}

// handleSendInvites marks a batch as sent and returns ready-to-tap links.
//
// Actually delivering through the WhatsApp Business API needs an approved
// template and a paid number, so this stops one step short of that on purpose
// and hands back wa.me links instead. The guest-facing half of the workflow --
// a unique link, resolved by the server, that lands on the RSVP screen -- is
// complete and real.
func (s *Server) handleSendInvites(w http.ResponseWriter, r *http.Request) {
	var body sendInvitesBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if len(body.GuestIDs) == 0 {
		s.badRequest(w, "pilih minimal satu tamu")
		return
	}

	guests, err := s.store.AdminGuests(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byID := make(map[string]model.AdminGuest, len(guests))
	for _, g := range guests {
		byID[g.ID] = g
	}

	template := waTemplate
	if set, err := s.store.Settings(r.Context()); err == nil && strings.TrimSpace(set.InviteTemplate) != "" {
		template = set.InviteTemplate
	}

	plans := make([]invitePlan, 0, len(body.GuestIDs))
	for _, id := range body.GuestIDs {
		g, ok := byID[id]
		if !ok {
			s.badRequest(w, "tamu tidak ditemukan: "+id)
			return
		}
		link := s.inviteURL(g.InviteToken, g.Name)
		msg := strings.NewReplacer("{nama}", g.Name, "{link}", link).Replace(template)
		plans = append(plans, invitePlan{
			GuestID: g.ID,
			Name:    g.Name,
			Phone:   g.Phone,
			URL:     link,
			Message: msg,
			WaURL:   "https://wa.me/" + waNumber(g.Phone) + "?text=" + url.QueryEscape(msg),
		})
	}

	if err := s.store.MarkInvite(r.Context(), body.GuestIDs, model.InviteSent, "", rundownMinute(time.Now())); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyStageRoom)
	s.writeJSON(w, http.StatusOK, plans)
}

// handleRevokeInvite turns a link off.
//
// The guest row stays, so their check-in, their cards and their questions
// survive -- this kills the credential, not the person.
func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.RevokeInvite(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAdminGuests)
	s.writeJSON(w, http.StatusOK, map[string]any{"guestId": id, "revoked": true})
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyDashboard, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		return s.store.Dashboard(ctx)
	})
}

func (s *Server) handleAdminFeedback(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyFeedback, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		perEvent, err := s.store.EventFeedback(ctx)
		if err != nil {
			return nil, err
		}
		final, err := s.store.FinalFeedback(ctx)
		if err != nil {
			return nil, err
		}
		if perEvent == nil {
			perEvent = map[string][]model.EventFeedback{}
		}
		if final == nil {
			final = []model.FinalFeedback{}
		}
		return map[string]any{"events": perEvent, "final": final}, nil
	})
}

type announcementBody struct {
	Title  string  `json:"title"`
	Body   string  `json:"body"`
	RoomID *string `json:"roomId"`
}

func (s *Server) handleAddAnnouncement(w http.ResponseWriter, r *http.Request) {
	s.addAnnouncement(w, r, "panitia")
}

func (s *Server) addAnnouncement(w http.ResponseWriter, r *http.Request, source string) {
	var body announcementBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	body.Title = strings.TrimSpace(body.Title)
	body.Body = strings.TrimSpace(body.Body)
	if body.Title == "" || body.Body == "" {
		s.badRequest(w, "judul dan isi wajib diisi")
		return
	}

	a, err := s.store.AddAnnouncement(r.Context(), model.Announcement{
		Title: body.Title, Body: body.Body, RoomID: body.RoomID, At: rundownMinute(time.Now()), Source: source,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAnnouncements)
	// The MC's phone shows committee announcements too.
	s.publishStage(r.Context(), stageEvent{Kind: "announcement"})
	s.writeJSON(w, http.StatusCreated, a)
}

// handleStageAnnouncement is the MC announcing from the stage. It lands in
// the same table as the committee's, so guests see both in one place.
func (s *Server) handleStageAnnouncement(w http.ResponseWriter, r *http.Request) {
	s.addAnnouncement(w, r, "mc")
}

// handleResendAnnouncement moves an announcement to now, which guests' phones
// read as new and show again.
func (s *Server) handleResendAnnouncement(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.ResendAnnouncement(r.Context(), r.PathValue("id"), rundownMinute(time.Now()))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAnnouncements)
	s.publishStage(r.Context(), stageEvent{Kind: "announcement"})
	s.writeJSON(w, http.StatusOK, a)
}

// handleSaveRundown replaces the rundown with what the committee just edited.
func (s *Server) handleSaveRundown(w http.ResponseWriter, r *http.Request) {
	var sessions []model.Session
	if err := decode(w, r, &sessions); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if len(sessions) == 0 {
		s.badRequest(w, "rundown tidak boleh kosong")
		return
	}
	for _, s2 := range sessions {
		if s2.ID == "" || s2.Title == "" {
			s.badRequest(w, "setiap sesi butuh id dan judul")
			return
		}
		if s2.End <= s2.Start {
			s.badRequest(w, "sesi "+s2.ID+": jam selesai harus setelah jam mulai")
			return
		}
	}

	if err := s.store.SaveSessions(r.Context(), sessions); err != nil {
		s.fail(w, r, err)
		return
	}
	// keyEvent carries startsAt, which is the first session's start time, so
	// moving the opening has to clear it too -- otherwise the countdown on
	// guests' phones keeps counting to the old one.
	s.invalidate(r.Context(), keyAgenda, keyEvent)
	// The MC side shows a "rundown changed" marker; this is what sets it.
	s.publishStage(r.Context(), stageEvent{Kind: "rundown"})
	s.writeJSON(w, http.StatusOK, sessions)
}

// inviteURL renders a token as the link the guest actually opens.
//
// The guest's name is put in front of the token, the way a paper invitation
// has a name on it: /u/budi-santoso-k4m2p9xr7tn3vq1w. It is decoration -- the
// token alone decides who the link belongs to, and the server never reads the
// slug -- but it is decoration that matters, because this URL is the first
// thing the guest sees in WhatsApp.
//
// Parsing stays unambiguous because tokens are exactly 16 characters from an
// alphabet with no dash in it (see token.ValidInviteShape): whatever the name
// contains, the token is the last 16 characters. The app's /u/ route splits
// it there.
func (s *Server) inviteURL(tok, name string) string {
	if tok == "" {
		return ""
	}
	if slug := nameSlug(name); slug != "" {
		return s.cfg.AppBaseURL + "/u/" + slug + "-" + tok
	}
	return s.cfg.AppBaseURL + "/u/" + tok
}

// nameSlug reduces a guest's name to lowercase ASCII words joined by dashes,
// dropping anything else. A name that survives as nothing -- one written in a
// script this does not transliterate -- returns "", and the link is simply
// the bare token rather than a row of dashes.
func nameSlug(name string) string {
	var b strings.Builder
	lastDash := true // leading dashes are never wanted
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

var phoneShape = regexp.MustCompile(`^08[0-9]{8,11}$`)

// normalizePhone mirrors normalizePhone in src/admin/invite.ts: keep digits
// (and a leading +), turn +62/62 -- or +620/620 -- into a leading 0, and
// return "" if the result cannot be an Indonesian mobile number. Digits only,
// because a number pasted from WhatsApp carries invisible direction marks.
func normalizePhone(raw string) string {
	s := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '+' {
			return r
		}
		return -1
	}, raw)
	s = strings.TrimPrefix(s, "+")
	if strings.HasPrefix(s, "62") {
		s = "0" + strings.TrimPrefix(s[2:], "0")
	}
	if !phoneShape.MatchString(s) {
		return ""
	}
	return s
}

// waNumber converts 08... to the 62... form wa.me expects.
func waNumber(phone string) string {
	if strings.HasPrefix(phone, "0") {
		return "62" + phone[1:]
	}
	return phone
}

// handleDeleteAnnouncement takes an announcement back; it leaves guests'
// Notifikasi on their next read.
func (s *Server) handleDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAnnouncement(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAnnouncements)
	s.publishStage(r.Context(), stageEvent{Kind: "announcement"})
	w.WriteHeader(http.StatusNoContent)
}
