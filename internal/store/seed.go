package store

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/token"
)

// This file mirrors the frontend fixtures one for one, so the API serves the
// same data the React app already shows:
//
//	seedNames, buildGuests -> ADMIN_GUESTS in src/admin/data.ts
//	seedProfiles           -> GUESTS in src/data/guests.ts
//	seedSessions           -> AGENDA + EXTRA in src/data/agenda.ts, src/stage/data.ts
//	seedPeople             -> PEOPLE in src/stage/data.ts
//
// Keeping them identical means switching the frontend from its fixtures to the
// API changes nothing on screen, so anything that *does* change is a real bug
// rather than different sample data.

// seedNames is NAMES, in order: the index drives every derived field below,
// exactly as it does in the TypeScript.
var seedNames = []string{
	"David", "Nanda", "Azwar", "Adam", "Sinta", "Raka", "Laras", "Dimas", "Putri", "Bayu", "Citra", "Fajar",
	"Gita", "Hana", "Ilham", "Joko", "Kirana", "Lutfi", "Maya", "Nadia", "Oka", "Pandu", "Rani", "Sekar",
	"Tegar", "Ulfa", "Vina", "Wulan", "Yoga", "Zahra", "Arya", "Bima",
}

// profile is the part of a guest that other guests get to see on a kenalan
// card: no phone, no email.
type profile struct {
	Role     string
	Question string
	// Code is the 4-digit code another guest types to collect this card.
	Code string
	// Outfit is a label from src/data/outfits.ts.
	Outfit string
}

// seedProfiles covers the guests GUESTS already describes, plus David, who is
// the guest the app plays as (MY_KENALAN_CODE in src/config.ts).
var seedProfiles = map[string]profile{
	"david": {Role: "Tamu", Question: "Kalau bisa pindah kantor ke kota mana pun, pilih mana?", Code: "4821", Outfit: "Hoodie"},
	"nanda": {Role: "Ilustrator", Question: "Karakter kartun apa yang paling membekas waktu kecil?", Code: "1357", Outfit: "Hoodie"},
	"azwar": {Role: "Product", Question: "Kalau kerja dari kantor ini, kamu paling suka duduk di ruangan mana?", Code: "2468", Outfit: "Kemeja"},
	"adam":  {Role: "Developer", Question: "Lagu apa yang lagi sering kamu putar minggu ini?", Code: "1122", Outfit: "Kaos Potara"},
	"sinta": {Role: "Tamu", Question: "Menu omakase apa yang paling pengin kamu coba?", Code: "3344", Outfit: "Topi"},
	"raka":  {Role: "Tamu", Question: "Kalau Potara punya maskot kedua, hewan apa yang cocok?", Code: "5566", Outfit: "Kacamata"},
	"laras": {Role: "Tamu", Question: "Hal kecil apa yang bikin harimu jadi baik?", Code: "7788", Outfit: "Syal"},
	"dimas": {Role: "Tamu", Question: "Tempat makan favoritmu di dekat sini apa?", Code: "9900", Outfit: "Jaket"},
}

// fallbackQuestions are handed out round-robin to the guests GUESTS does not
// name, so every seeded guest has a usable kenalan card.
var fallbackQuestions = []string{
	"Minuman apa yang wajib ada di meja kerjamu?",
	"Film terakhir yang kamu tonton sampai habis?",
	"Kalau libur sebulan, mau ke mana?",
	"Hal apa yang baru kamu pelajari bulan ini?",
	"Playlist buat kerja: musik apa?",
}

// profileFor returns the fixture profile when there is one, and a deterministic
// stand-in otherwise. Deterministic matters: the codes are typed in by hand
// during testing, so they must be the same on every restart.
func profileFor(id string, i int) profile {
	if p, ok := seedProfiles[id]; ok {
		return p
	}
	return profile{
		Role:     "Tamu",
		Question: fallbackQuestions[i%len(fallbackQuestions)],
		// 4 digits, never starting at 0, and distinct per index.
		Code:   strconv.Itoa(1000 + (i*137)%9000),
		Outfit: "Hoodie",
	}
}

// guestRow is a guest as the store holds them: the admin view, plus the invite
// secret and the public profile.
type guestRow struct {
	model.AdminGuest
	profile profile
	// revoked kills the link without deleting the guest.
	revoked bool
}

// buildGuests reproduces the ADMIN_GUESTS mapping, index for index.
func buildGuests() ([]*guestRow, error) {
	rows := make([]*guestRow, 0, len(seedNames))
	for i, name := range seedNames {
		id := lower(name)

		rsvp := model.RsvpYes
		switch {
		case i%11 == 7:
			rsvp = model.RsvpNo
		case i%9 == 8:
			rsvp = model.RsvpUnset
		}

		// "here" is the sample rule for who has already arrived.
		here := rsvp == model.RsvpYes && i%6 != 5

		cards := 0
		if here {
			cards = []int{5, 3, 2, 5, 1, 4, 0, 5, 2}[i%9]
		}

		var checkIn *int
		checkInBy := ""
		if here {
			m := 9*60 + (i*7)%48
			checkIn = &m
			switch {
			case i%8 == 3:
				checkInBy = "panitia"
			case i%8 == 6:
				checkInBy = "kode"
			default:
				checkInBy = "qr"
			}
		}

		reward := model.RewardCollecting
		var claimedAt *int
		if cards >= 5 {
			if i%2 == 1 {
				reward = model.RewardClaimed
				c := 9*60 + 40 + i%9
				claimedAt = &c
			} else {
				reward = model.RewardReady
			}
		}

		invite := model.InviteRead
		if rsvp == model.RsvpUnset && i%2 == 0 {
			invite = model.InviteSent
		}
		invitedAt := 8*60 + i%50

		tok, err := token.NewInvite()
		if err != nil {
			return nil, fmt.Errorf("mint seed invite token: %w", err)
		}

		// A guest who is "Hadir" has by definition been through the
		// customisation screen, so they have an outfit. One who has not
		// arrived has none, and the admin list is meant to show that
		// difference.
		p := profileFor(id, i)
		var outfit model.Outfit
		if here {
			outfit = model.Outfit{Baju: p.Outfit}
		}

		rows = append(rows, &guestRow{
			AdminGuest: model.AdminGuest{
				ID:    id,
				Name:  name,
				Email: id + "@contoh.id",
				Phone: "0812" + strconv.Itoa(3000000 + i*73519)[:7],
				Rsvp:  rsvp,

				CheckIn:   checkIn,
				CheckInBy: checkInBy,
				// Two guests are mid-customisation right now, so they are
				// checked in but not yet "Hadir".
				Hadir: here && i != 4 && i != 13,

				Invite:    invite,
				InvitedAt: &invitedAt,

				Cards:         cards,
				Reward:        reward,
				ClaimedAt:     claimedAt,
				FinalFeedback: false,
				Outfit:        outfit,

				InviteToken: tok,
			},
			profile: p,
		})
	}
	return rows, nil
}

// seedNotes are the lines written on a collected kenalan card, cycled so every
// seeded card has something on it. The first three are SAMPLE_COLLECTED from
// src/data/guests.ts.
var seedNotes = []string{
	"Ruang Kerja Dalam, katanya paling tenang.",
	"Lagi muter soundtrack game terus.",
	"Salmon aburi!",
	"Katanya paling suka pojok dekat jendela.",
	"Mau coba bikin maskot kedua Potara.",
	"Kopi item, tanpa gula.",
}

// seedCards turns each guest's intended card count into actual card rows.
//
// This exists because the count and the rows have to agree. The frontend keeps
// them as two unrelated fixtures (ADMIN_GUESTS.cards is a number,
// SAMPLE_COLLECTED is a list), which is harmless in a mockup but not here: the
// Postgres store derives the count by counting rows, so if the seed only set
// the number the two backends would report different totals for the same
// guest. One source of truth -- the rows -- and the count follows from them.
//
// Returns cards[owner][from] = note.
func seedCards(rows []*guestRow) map[string]map[string]string {
	out := make(map[string]map[string]string, len(rows))
	for i, owner := range rows {
		want := owner.Cards
		if want <= 0 {
			continue
		}
		held := make(map[string]string, want)
		// Walk forward from this guest, so the picks are deterministic and
		// nobody collects their own card.
		for step := 1; len(held) < want && step < len(rows); step++ {
			from := rows[(i+step)%len(rows)]
			if from.ID == owner.ID {
				continue
			}
			held[from.ID] = seedNotes[(i+step)%len(seedNotes)]
		}
		out[owner.ID] = held
		// The count is now whatever the rows say, which is the only number
		// either backend will ever report.
		owner.Cards = len(held)
	}
	return out
}

// seedSessions is AGENDA joined with EXTRA.
func seedSessions() []model.Session {
	t := func(h, m int) int { return h*60 + m }
	return []model.Session{
		{AgendaItem: model.AgendaItem{ID: "pembukaan", Start: t(9, 0), End: t(9, 30), Title: "Pembukaan", RoomID: "halaman-depan"}, PIC: "Azwar", QnA: false},
		{AgendaItem: model.AgendaItem{ID: "talkshow", Start: t(9, 30), End: t(10, 30), Title: "Talkshow", RoomID: "ruang-omakase"}, PIC: "Adam", SpeakerID: "ardi", QnA: true},
		{AgendaItem: model.AgendaItem{ID: "pengenalan", Start: t(10, 30), End: t(11, 15), Title: "Pengenalan kantor", RoomID: "ruang-kerja-utama"}, PIC: "Nanda", QnA: true},
		{AgendaItem: model.AgendaItem{ID: "makan", Start: t(11, 30), End: t(12, 45), Title: "Makan siang bersama", RoomID: "ruang-omakase"}, PIC: "Azwar", QnA: false},
		{AgendaItem: model.AgendaItem{ID: "penutupan", Start: t(12, 45), End: t(13, 30), Title: "Foto bersama & penutupan", RoomID: "halaman-depan"}, PIC: "David", QnA: false},
	}
}

// seedPeople is PEOPLE: who stands on stage.
func seedPeople() []model.Person {
	return []model.Person{
		{ID: "nadia", Name: "Nadia Putri", Role: "mc", Title: "MC", Phone: "081234561114"},
		{ID: "ardi", Name: "Ardi Nugraha", Role: "pembicara", Title: "Guest star talkshow", Phone: "081377705512"},
	}
}

// seedQuestions is SAMPLE_QUESTIONS. `mine` is left false here because it is
// decided per request from the caller's token, never stored.
func seedQuestions() []model.Question {
	author := func(s string) *string { return &s }
	return []model.Question{
		{ID: "q1", EventID: "talkshow", Text: "Kenapa akhirnya pilih pindah ke kantor ini?", Author: author("Sinta"), AuthorGuestID: "sinta", Status: model.QuestionAnswered, At: 9*60 + 36},
		{ID: "q2", EventID: "talkshow", Text: "Ruangan mana yang paling susah didesain?", Author: nil, AuthorGuestID: "laras", Status: model.QuestionRead, At: 9*60 + 41},
		{ID: "q3", EventID: "talkshow", Text: "Apa rencana Potara setahun ke depan di rumah baru?", Author: author("David"), AuthorGuestID: "david", Status: model.QuestionSent, At: 9*60 + 47},
		{ID: "q4", EventID: "talkshow", Text: "Ada ruang buat tamu kerja bareng nggak nanti?", Author: author("Raka"), AuthorGuestID: "raka", Status: model.QuestionSent, At: 9*60 + 48},
	}
}

// seedAnnouncements is SAMPLE_ANNOUNCEMENTS.
func seedAnnouncements() []model.Announcement {
	room := func(s string) *string { return &s }
	return []model.Announcement{
		{ID: "a1", Title: "Selamat datang!", Body: "Check-in di pintu, lalu jelajahi 5 ruangan Potara.", RoomID: room("halaman-depan"), At: 9*60 + 2, Recipients: 6, Read: 6},
		{ID: "a2", Title: "Talkshow segera mulai", Body: "Kursi di Ruang Omakase masih banyak, yuk merapat.", RoomID: room("ruang-omakase"), At: 9*60 + 25, Recipients: 18, Read: 14},
	}
}

// lower turns a name into an id: ASCII lowercase, with every run of anything
// else collapsed to a single dash.
//
// ASCII by hand rather than strings.ToLower, so an id does not depend on the
// server's locale. The dashes matter because a guest id travels in a URL path
// (DELETE /api/admin/guests/{id}/invite): "Budi Santoso" lowercased alone
// gives "budi santoso", which only works if every caller remembers to encode
// the spaces, and silently fails as a malformed URL when one does not.
//
// Seeded names are single ASCII words, so their ids are unchanged by this.
func lower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := true // no leading dash
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
			lastDash = false
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// defaultSettings mirrors the column defaults in migrations/004_settings.sql,
// so the in-memory store and a fresh Postgres start from the same row. The
// two must agree: an event tested against one and run on the other should not
// have a different door code.
func defaultSettings() model.Settings {
	return model.Settings{
		EventName:       "House Moving Day",
		Date:            "2026-11-08",
		Place:           model.Place{Name: "Kantor baru Potara"},
		DoorCode:        "1108",
		PanitiaPIN:      "2026",
		Countdown:       true,
		FinalFeedbackAt: 13*60 + 30,
		CodesChangedAt:  7*60 + 45,
	}
}
