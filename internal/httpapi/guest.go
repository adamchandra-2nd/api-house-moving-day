package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
)

// Reads and writes for a signed-in guest. Everything here sits behind
// inviteAuth, so the caller is always a known guest.

// questionMax mirrors QUESTION_MAX in src/data/questions.ts. The client
// enforces it for the character counter; the server enforces it because the
// client cannot be trusted to.
const questionMax = 200

// handleGuests is the kenalan list: who is here, as a card.
//
// Cached and ETagged. This is the single most-read endpoint of the day -- a
// room full of phones polling it -- and it carries no per-caller fields, so
// one cached copy serves everyone.
func (s *Server) handleGuests(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyGuests, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		guests, err := s.store.PresentGuests(ctx)
		if err != nil {
			return nil, err
		}
		if guests == nil {
			// An empty list, not null: the frontend maps over this.
			guests = []model.Guest{}
		}
		return guests, nil
	})
}

// handleAgenda is the rundown.
func (s *Server) handleAgenda(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyAgenda, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		sessions, err := s.store.Sessions(ctx)
		if err != nil {
			return nil, err
		}
		if sessions == nil {
			sessions = []model.Session{}
		}
		return sessions, nil
	})
}

func (s *Server) handleAnnouncements(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyAnnouncements, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		list, err := s.store.Announcements(ctx)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []model.Announcement{}
		}
		return list, nil
	})
}

func (s *Server) handlePeople(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyPeople, time.Minute, func(ctx context.Context) (any, error) {
		list, err := s.store.People(ctx)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []model.Person{}
		}
		return list, nil
	})
}

// handleQuestions lists questions for a session.
//
// Deliberately not cached: `mine` differs per caller, so a shared cached copy
// would hand one guest another guest's edit buttons. Questions are also the
// fastest-changing data of the day, which is exactly when a cache earns least.
func (s *Server) handleQuestions(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	list, err := s.store.Questions(r.Context(), r.URL.Query().Get("eventId"))
	if err != nil {
		s.fail(w, r, err)
		return
	}

	out := make([]model.Question, 0, len(list))
	for _, q := range list {
		q.Mine = q.AuthorGuestID == g.ID
		out = append(out, q)
	}
	s.writeJSON(w, http.StatusOK, out)
}

type sendQuestionBody struct {
	EventID string `json:"eventId"`
	Text    string `json:"text"`
	// Author nil or "" means send it anonymously.
	Author *string `json:"author"`
	// At is the rundown minute the guest sent it. Optional: the server fills
	// it in, because a phone with a wrong clock would otherwise file its
	// question in the middle of a session that already finished.
	At int `json:"at,omitempty"`
}

func (s *Server) handleSendQuestion(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body sendQuestionBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		s.badRequest(w, "pertanyaan masih kosong")
		return
	}
	if len([]rune(body.Text)) > questionMax {
		s.badRequest(w, "pertanyaan terlalu panjang")
		return
	}
	if body.EventID == "" {
		s.badRequest(w, "eventId wajib diisi")
		return
	}

	// An anonymous question still records who sent it, so the author can edit
	// or delete their own; the name is simply not shown.
	author := body.Author
	if author != nil && strings.TrimSpace(*author) == "" {
		author = nil
	}

	s.idempotent(w, r, "q:"+g.ID, func() (any, error) {
		q, err := s.store.AddQuestion(r.Context(), model.Question{
			EventID:       body.EventID,
			Text:          body.Text,
			Author:        author,
			AuthorGuestID: g.ID,
			Status:        model.QuestionSent,
			At:            rundownMinute(time.Now()),
		})
		if err != nil {
			return nil, err
		}
		s.invalidate(r.Context(), keyDashboard)
		// Push it to the MC and speaker phones immediately: a question that
		// arrives after the session has moved on is useless.
		s.publishStage(r.Context(), stageEvent{Kind: "question", Question: &q})

		q.Mine = true
		return q, nil
	})
}

type editQuestionBody struct {
	Text string `json:"text"`
}

func (s *Server) handleEditQuestion(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)
	id := r.PathValue("id")

	var body editQuestionBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if body.Text == "" {
		s.badRequest(w, "pertanyaan masih kosong")
		return
	}
	if len([]rune(body.Text)) > questionMax {
		s.badRequest(w, "pertanyaan terlalu panjang")
		return
	}

	// The store checks ownership and refuses once the MC has read it, so the
	// 403 and the 409 are decided in one place rather than here.
	q, err := s.store.EditQuestion(r.Context(), id, g.ID, body.Text)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishStage(r.Context(), stageEvent{Kind: "question", Question: &q})

	q.Mine = true
	s.writeJSON(w, http.StatusOK, q)
}

func (s *Server) handleDeleteQuestion(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)
	id := r.PathValue("id")

	if err := s.store.DeleteQuestion(r.Context(), id, g.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.invalidate(r.Context(), keyDashboard)
	s.publishStage(r.Context(), stageEvent{Kind: "question-deleted", QuestionID: id})
	s.writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

type collectCardBody struct {
	// GuestID is whose card this is. Optional when Code is given.
	GuestID string `json:"guestId,omitempty"`
	// Code is the 4-digit code the other guest reads out. Either this or
	// guestId identifies the card.
	Code string `json:"code,omitempty"`
	Note string `json:"note"`
}

type collectCardResult struct {
	GuestID string `json:"guestId"`
	Note    string `json:"note"`
	// Cards is the collector's new total, so the progress bar can move without
	// a second request.
	Cards int `json:"cards"`
	// Reward is true once there are enough cards to claim the prize.
	Reward bool `json:"reward"`
}

// handleCollectCard records a kenalan.
//
// Accepting the 4-digit code as well as a guest id is what makes this usable
// at the event: the guest reads a number off their friend's screen rather than
// scanning anything.
func (s *Server) handleCollectCard(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body collectCardBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}

	fromID := body.GuestID
	if fromID == "" {
		if body.Code == "" {
			s.badRequest(w, "isi kode kenalan atau guestId")
			return
		}
		guests, err := s.store.PresentGuests(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, other := range guests {
			if other.Code == body.Code {
				fromID = other.ID
				break
			}
		}
		if fromID == "" {
			s.writeJSON(w, http.StatusNotFound, errBody{Error: "kode kenalan tidak ditemukan", Code: "bad_code"})
			return
		}
	}

	s.idempotent(w, r, "card:"+g.ID, func() (any, error) {
		count, err := s.store.AddCard(r.Context(), g.ID, fromID, strings.TrimSpace(body.Note))
		if err != nil {
			return nil, err
		}
		s.invalidate(r.Context(), keyAdminGuests, keyDashboard, keyStageRoom)
		return collectCardResult{GuestID: fromID, Note: body.Note, Cards: count, Reward: count >= 5}, nil
	})
}

func (s *Server) handleMyCards(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	cards, err := s.store.Cards(r.Context(), g.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if cards == nil {
		cards = []model.Card{}
	}
	s.writeJSON(w, http.StatusOK, cards)
}

type eventFeedbackBody struct {
	EventID string `json:"eventId"`
	Rating  int    `json:"rating"`
	Text    string `json:"text,omitempty"`
}

func (s *Server) handleEventFeedback(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body eventFeedbackBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if body.Rating < 1 || body.Rating > 5 {
		s.badRequest(w, "rating harus 1 sampai 5")
		return
	}
	if body.EventID == "" {
		s.badRequest(w, "eventId wajib diisi")
		return
	}

	s.idempotent(w, r, "fb:"+g.ID, func() (any, error) {
		f := model.EventFeedback{EventID: body.EventID, Rating: body.Rating, Text: body.Text, At: rundownMinute(time.Now())}
		if err := s.store.AddEventFeedback(r.Context(), g.ID, f); err != nil {
			return nil, err
		}
		s.invalidate(r.Context(), keyFeedback, keyDashboard, keyStageRoom)
		return f, nil
	})
}

type finalFeedbackBody struct {
	Rating int      `json:"rating"`
	Note   string   `json:"note,omitempty"`
	Moods  []string `json:"moods,omitempty"`
}

func (s *Server) handleFinalFeedback(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)

	var body finalFeedbackBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if body.Rating < 1 || body.Rating > 5 {
		s.badRequest(w, "rating harus 1 sampai 5")
		return
	}

	s.idempotent(w, r, "final:"+g.ID, func() (any, error) {
		f := model.FinalFeedback{Rating: body.Rating, Note: body.Note, Moods: body.Moods, At: rundownMinute(time.Now())}
		if err := s.store.AddFinalFeedback(r.Context(), g.ID, f); err != nil {
			return nil, err
		}
		s.invalidate(r.Context(), keyFeedback, keyDashboard, keyAdminGuests)
		return f, nil
	})
}

// ---- "Ajak kenalan" ----

// kenalanCooldown is how long before the same guest can be asked again: long
// enough that it is not a doorbell, short enough to try once more later.
const kenalanCooldown = 10 * time.Minute

type kenalanInviteBody struct {
	GuestID string `json:"guestId"`
}

// handleKenalanInvite lets the guest know someone would like to meet them.
// No location goes with it: the room shown in the app is not necessarily
// where either of them is standing.
func (s *Server) handleKenalanInvite(w http.ResponseWriter, r *http.Request) {
	g := guestOf(r)
	var body kenalanInviteBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if body.GuestID == "" || body.GuestID == g.ID {
		s.badRequest(w, "pilih tamu lain untuk diajak kenalan")
		return
	}
	inv, err := s.store.AddKenalanInvite(r.Context(), g.ID, body.GuestID, rundownMinute(time.Now()), kenalanCooldown)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	inv.FromName = g.Name
	s.writeJSON(w, http.StatusCreated, inv)
}

type kenalanInvitesResult struct {
	Received []model.KenalanInvite `json:"received"`
	Sent     []model.KenalanInvite `json:"sent"`
}

// handleKenalanInvites is this guest's invites: received, for Notifikasi,
// and the latest one sent to each guest, for the "Ajak lagi" countdown.
// Not cached: it differs per caller.
func (s *Server) handleKenalanInvites(w http.ResponseWriter, r *http.Request) {
	received, sent, err := s.store.KenalanInvites(r.Context(), guestOf(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, kenalanInvitesResult{Received: received, Sent: sent})
}
