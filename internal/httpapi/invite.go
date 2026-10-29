package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
)

// The guest side of a personal invite link.
//
// The link the committee sends is APP_BASE_URL/u/<token>. That path belongs to
// the React app, which stores the token and then calls these endpoints with it
// as a bearer credential. The token deliberately never travels in an API path,
// because paths get logged and this one is a credential.

// handleInvite resolves a link into everything the RSVP screen needs.
//
// This is the first request a guest's phone ever makes, so it is also the one
// most likely to happen on a bad connection outside the venue. It is a single
// round trip and it is cached per token rather than per path, so a guest
// reloading the page repeatedly does not hit Postgres each time.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	cards, err := s.store.Cards(r.Context(), g.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := s.store.Settings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusOK, model.Invite{
		GuestID:   g.ID,
		Name:      g.Name,
		Rsvp:      g.Rsvp,
		CheckedIn: g.CheckIn != nil,
		Hadir:     g.Hadir,
		Code:      s.kenalanCode(r, g.ID),
		Cards:     len(cards),
		Outfit:    g.Outfit,
		Countdown: set.Countdown,
		Event:     s.eventInfo(r.Context(), set),
	})
}

// host is not on the Pengaturan page, so it stays a constant rather than
// pretending to be editable.
const host = "Potara Studio"

// publicEvent is what the app can know before anyone has opened their link.
//
// Deliberately a different shape from the settings row rather than that row
// with fields removed: the row holds the door code and the committee PIN, and
// a response built by omission is one careless edit away from including them.
// This one can only ever carry what is written on the invitation anyway.
type publicEvent struct {
	model.EventInfo
	Countdown bool `json:"countdown"`
}

// handleEvent answers without a credential.
//
// The guest app needs the event's name, date and the countdown switch on its
// very first screen, which is before a guest has necessarily opened their
// personal link -- from a bookmark, or with the app installed from a previous
// visit. Gating it behind an invite token meant those cases silently fell
// back to the values compiled into the bundle, which is how a countdown the
// committee had switched off kept showing.
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyEvent, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		set, err := s.store.Settings(ctx)
		if err != nil {
			return nil, err
		}
		return publicEvent{EventInfo: s.eventInfo(ctx, set), Countdown: set.Countdown}, nil
	})
}

// eventInfo describes the event as the committee has configured it.
//
// These were literals until now, which meant the Pengaturan page could rename
// the event, move it or change its date and no guest would ever see it -- the
// same shape of bug as the door code.
func (s *Server) eventInfo(ctx context.Context, set model.Settings) model.EventInfo {
	day, err := time.ParseInLocation("2006-01-02", set.Date, eventZone)
	if err != nil {
		// A row that cannot be parsed should not take the invite screen down
		// with it: the guest gets the raw date and everything else works.
		return model.EventInfo{Name: set.EventName, Host: host, DateLabel: set.Date, Place: set.Place.Name}
	}

	// The countdown counts to when the event actually starts, which is the
	// first item in the rundown the committee edits -- not a fixed 09:00 that
	// would quietly lie after they moved the opening.
	start := day.Add(9 * time.Hour)
	if sessions, err := s.store.Sessions(ctx); err == nil && len(sessions) > 0 {
		first := sessions[0].Start
		for _, x := range sessions {
			if x.Start < first {
				first = x.Start
			}
		}
		start = day.Add(time.Duration(first) * time.Minute)
	}

	return model.EventInfo{
		Name:      set.EventName,
		Host:      host,
		DateLabel: indonesianDate(day),
		Place:     set.Place.Name,
		StartsAt:  start.Format(time.RFC3339),
	}
}

var (
	idDays   = [...]string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}
	idMonths = [...]string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}
)

// indonesianDate writes "Minggu, 8 November 2026".
//
// Done here rather than in the browser because every guest should read the
// same words: toLocaleDateString depends on the phone's locale, and a phone
// set to English would show "Sunday, November 8" on a screen that is
// otherwise entirely in Indonesian.
func indonesianDate(t time.Time) string {
	return fmt.Sprintf("%s, %d %s %d", idDays[int(t.Weekday())], t.Day(), idMonths[int(t.Month())-1], t.Year())
}

// kenalanCode finds this guest's own 4-digit code, the one other guests type
// in to collect their card. It comes from the public guest list, which is
// cached, so this costs nothing.
func (s *Server) kenalanCode(r *http.Request, guestID string) string {
	guests, err := s.store.PresentGuests(r.Context())
	if err != nil {
		return ""
	}
	for _, g := range guests {
		if g.ID == guestID {
			return g.Code
		}
	}
	return ""
}

type rsvpBody struct {
	Rsvp model.Rsvp `json:"rsvp"`
}

// handleRsvp records yes or no.
//
// Idempotent, because this is the write most likely to be sent from a guest
// standing outside with one bar of signal: tapping "Ya" twice, or a replay
// from the offline queue, must not produce two different answers.
func (s *Server) handleRsvp(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body rsvpBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if body.Rsvp != model.RsvpYes && body.Rsvp != model.RsvpNo {
		s.badRequest(w, `rsvp harus "ya" atau "tidak"`)
		return
	}

	s.idempotent(w, r, "rsvp:"+g.ID, func() (any, error) {
		if err := s.store.SetRsvp(r.Context(), g.ID, body.Rsvp); err != nil {
			return nil, err
		}
		// The committee's list and dashboard both show RSVP counts.
		s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyStageRoom)
		return rsvpBody{Rsvp: body.Rsvp}, nil
	})
}

type checkInBody struct {
	// By is "qr", "kode" or "panitia", matching AdminGuest.checkInBy.
	By string `json:"by"`
	// Code is the door code for "kode", or the committee PIN for "panitia".
	Code string `json:"code,omitempty"`
}

type checkInResult struct {
	CheckedIn bool   `json:"checkedIn"`
	At        int    `json:"at"`
	By        string `json:"by"`
}

// handleCheckIn is the door.
//
// The codes are verified here rather than in the browser. In the prototype the
// door code lives in src/config.ts, which means anybody who opens devtools can
// read it; once check-in decides who counts as present, that check has to be
// somewhere the guest cannot edit.
func (s *Server) handleCheckIn(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body checkInBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}

	// From the settings row, so a code the committee rotates on the
	// Pengaturan page takes effect at the door immediately.
	doorCode, panitiaPIN := s.doorCodes(r.Context())

	switch body.By {
	case "qr":
		// The QR code *is* the invite link, so holding a valid token is the
		// proof. Nothing further to check.
	case "kode":
		if body.Code != doorCode {
			s.writeJSON(w, http.StatusForbidden, errBody{Error: "kode pintu salah", Code: "bad_code"})
			return
		}
	case "panitia":
		if body.Code != panitiaPIN {
			s.writeJSON(w, http.StatusForbidden, errBody{Error: "PIN panitia salah", Code: "bad_code"})
			return
		}
	default:
		s.badRequest(w, `by harus "qr", "kode" atau "panitia"`)
		return
	}

	minute := rundownMinute(time.Now())
	s.idempotent(w, r, "checkin:"+g.ID, func() (any, error) {
		if err := s.store.CheckIn(r.Context(), g.ID, minute, body.By); err != nil {
			return nil, err
		}
		s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyGuests, keyStageRoom)
		return checkInResult{CheckedIn: true, At: minute, By: body.By}, nil
	})
}

type hadirBody struct {
	Hadir bool `json:"hadir"`
	// Outfit is the character the guest just finished. It travels with
	// `hadir` rather than in a request of its own because it is the same
	// event: the customisation screen's confirm button is what makes a guest
	// "Hadir", and a guest cannot be hadir without having built something.
	// One request also means one thing to retry on a bad connection.
	Outfit model.Outfit `json:"outfit"`
}

// handleHadir flips "Sedang masuk" to "Hadir" and saves the character.
//
// The app calls it when the guest finishes building their character, which is
// what the committee treats as actually arrived -- a guest still in
// customisation has no character to show on a kenalan card yet.
func (s *Server) handleHadir(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body hadirBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if g.CheckIn == nil {
		s.writeJSON(w, http.StatusConflict, errBody{Error: "check-in dulu di pintu", Code: "not_checked_in"})
		return
	}
	if cat, ok := body.Outfit.Valid(); !ok {
		s.badRequest(w, "pilihan "+cat+" tidak dikenal")
		return
	}

	s.idempotent(w, r, "hadir:"+g.ID, func() (any, error) {
		// Outfit first. If this request is retried after failing halfway, a
		// guest marked hadir with no character is the state that shows an
		// empty kenalan card to the room; a character saved against a guest
		// not yet hadir shows nobody anything.
		if err := s.store.SetOutfit(r.Context(), g.ID, body.Outfit); err != nil {
			return nil, err
		}
		if err := s.store.SetHadir(r.Context(), g.ID, body.Hadir); err != nil {
			return nil, err
		}
		// Becoming "Hadir" adds this guest to the kenalan list everyone reads.
		s.invalidate(r.Context(), keyGuests, keyAdminGuests, keyDashboard, keyStageRoom)
		return hadirBody{Hadir: body.Hadir, Outfit: body.Outfit}, nil
	})
}

type claimRewardBody struct {
	// PIN is typed by a committee member on the guest's phone, at the prize
	// table. It is the proof that a prize actually changed hands.
	PIN string `json:"pin"`
}

// handleClaimOwnReward is the voucher screen: the guest holds out their phone
// and the committee types their PIN into it.
//
// This was decided in the browser against a constant compiled into the app,
// which had two consequences. A PIN the committee rotated on the Pengaturan
// page kept being refused while the old one kept working. And nothing was
// recorded: the guest's row still said "siap", so the same prize could be
// claimed again on another phone and the committee's own list never knew a
// voucher had been redeemed.
//
// The eligibility check is the store's, not this handler's -- `where reward =
// 'siap'` -- so two committee phones on the same guest hand out one prize.
func (s *Server) handleClaimOwnReward(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body claimRewardBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}

	// Rate limited per guest: this is a short numeric PIN shown around a
	// prize table, and guessing should not be free.
	if !s.cache.Allow(r.Context(), "rl:claim:"+g.ID, loginTriesPerMinute, time.Minute) {
		s.writeJSON(w, http.StatusTooManyRequests, errBody{Error: "terlalu banyak percobaan, tunggu sebentar", Code: "rate_limited"})
		return
	}

	_, panitiaPIN := s.doorCodes(r.Context())
	if body.PIN != panitiaPIN {
		s.writeJSON(w, http.StatusForbidden, errBody{Error: "PIN panitia salah", Code: "bad_code"})
		return
	}

	minute := rundownMinute(time.Now())
	if err := s.store.ClaimReward(r.Context(), g.ID, minute); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyStageRoom)
	s.writeJSON(w, http.StatusOK, map[string]any{"reward": model.RewardClaimed, "claimedAt": minute})
}

// rundownMinute converts a real instant into the app's clock unit: minutes
// since midnight, in the event's own timezone.
//
// Everything on screen -- the rundown, check-in times, when a question was
// asked -- is in these minutes, so the conversion happens once, here, instead
// of being redone in every handler.
func rundownMinute(t time.Time) int {
	local := t.In(eventZone)
	return local.Hour()*60 + local.Minute()
}

// eventZone is Jakarta: the event happens in one room, in one timezone, and a
// server in another one must not shift the rundown.
var eventZone = time.FixedZone("WIB", 7*60*60)
