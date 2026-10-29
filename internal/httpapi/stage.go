package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
	"github.com/adamchandra-2nd/api-house-moving-day/internal/token"
)

// The MC and Performance sides.
//
// In the prototype these two share state through localStorage and the browser
// `storage` event, which only ever syncs tabs inside one browser -- so the
// two-tab demo works and two actual phones do not. Here the shared state lives
// on the server and is pushed over Server-Sent Events, which is what makes the
// MC's phone and the speaker's phone talk to each other for real.
//
// SSE rather than WebSockets on purpose: this traffic is almost entirely one
// way, SSE is plain HTTP so it survives hotel and venue proxies that break
// WebSocket upgrades, and the browser reconnects on its own after a dropped
// connection -- which on venue wifi is the normal case, not the exception.

// stageChannel is the single pub/sub channel. One channel is right here: there
// is one stage, and both stage sides plus the committee's bell want the same
// events.
const stageChannel = "stage"

// heartbeat keeps the stream alive. Proxies and phone radios drop idle
// connections, and a comment line is the cheapest way to stay warm.
const heartbeat = 20 * time.Second

type stageEvent struct {
	// Kind is "state", "question" or "question-deleted".
	Kind string `json:"kind"`

	Rev   int64           `json:"rev,omitempty"`
	State json.RawMessage `json:"state,omitempty"`

	Question   *model.Question `json:"question,omitempty"`
	QuestionID string          `json:"questionId,omitempty"`
}

// publishStage fans an event out to every connected stage client.
//
// Failures are logged and swallowed: a question that was written to Postgres
// must not be reported as failed because the notification did not go out. The
// clients re-read state when they reconnect.
func (s *Server) publishStage(ctx context.Context, ev stageEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		s.log.Error("marshal stage event", "err", err)
		return
	}
	if err := s.cache.Publish(ctx, stageChannel, body); err != nil {
		s.log.Warn("publish stage event", "kind", ev.Kind, "err", err)
	}
}

type stageLoginBody struct {
	Code string `json:"code"`
	// PersonID is which MC or speaker this is, from GET /api/stage/people.
	PersonID string `json:"personId"`
}

type stageLoginResult struct {
	Token  string       `json:"token"`
	Person model.Person `json:"person"`
}

// handleStageLogin exchanges the shared stage code for a signed session.
//
// Rate limited per IP, because this is a six-digit code typed on a phone: it
// is short enough to guess if guessing were free.
func (s *Server) handleStageLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cache.Allow(r.Context(), "rl:stage-login:"+clientIP(r), loginTriesPerMinute, time.Minute) {
		s.writeJSON(w, http.StatusTooManyRequests, errBody{Error: "terlalu banyak percobaan, tunggu sebentar", Code: "rate_limited"})
		return
	}

	var body stageLoginBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if body.Code != s.cfg.StageCode {
		s.writeJSON(w, http.StatusForbidden, errBody{Error: "kode salah", Code: "bad_code"})
		return
	}

	people, err := s.store.People(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var who model.Person
	for _, p := range people {
		if p.ID == body.PersonID {
			who = p
			break
		}
	}
	if who.ID == "" {
		s.badRequest(w, "personId tidak dikenal")
		return
	}

	// The role comes from the person record, not from the request: an MC code
	// holder must not be able to ask for the speaker's side by saying so.
	sess := token.Session{Subject: who.ID, Role: who.Role, Expires: time.Now().Add(stageSessionTTL)}
	s.writeJSON(w, http.StatusOK, stageLoginResult{Token: token.Sign(s.cfg.SessionSecret, sess), Person: who})
}

type stageStateBody struct {
	// Rev is the revision the client last read. The server rejects the write
	// if somebody else has written since, so neither side can silently
	// clobber the other mid-session.
	//
	// -1 means "replace whatever is there", which is what a review case
	// seeding the stage does on purpose.
	Rev   int64           `json:"rev"`
	State json.RawMessage `json:"state"`
}

type stageStateResult struct {
	Rev   int64           `json:"rev"`
	State json.RawMessage `json:"state"`
}

func (s *Server) handleStageState(w http.ResponseWriter, r *http.Request) {
	rev, state, err := s.store.StageState(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, stageStateResult{Rev: rev, State: state})
}

// handleSaveStageState writes the shared state and pushes it to the other side.
func (s *Server) handleSaveStageState(w http.ResponseWriter, r *http.Request) {
	var body stageStateBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	if len(body.State) == 0 {
		s.badRequest(w, "state wajib diisi")
		return
	}

	rev, err := s.store.SaveStageState(r.Context(), body.Rev, body.State)
	if err != nil {
		// On a lost race the current rev goes back in the response, so the
		// client can re-read and retry without a second request.
		s.writeJSON(w, http.StatusConflict, struct {
			errBody
			Rev int64 `json:"rev"`
		}{errBody{Error: "sisi lain sudah mengubah state ini", Code: "conflict"}, rev})
		return
	}

	s.publishStage(r.Context(), stageEvent{Kind: "state", Rev: rev, State: body.State})
	s.writeJSON(w, http.StatusOK, stageStateResult{Rev: rev, State: body.State})
}

type questionStatusBody struct {
	Status model.QuestionStatus `json:"status"`
}

// handleQuestionStatus is the MC marking a question read or answered. It is on
// the stage side, not the guest side: a guest must not be able to mark their
// own question answered.
func (s *Server) handleQuestionStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var body questionStatusBody
	if err := decode(w, r, &body); err != nil {
		s.badRequest(w, err.Error())
		return
	}
	switch body.Status {
	case model.QuestionSent, model.QuestionRead, model.QuestionAnswered:
	default:
		s.badRequest(w, `status harus "terkirim", "dibaca" atau "terjawab"`)
		return
	}

	if err := s.store.SetQuestionStatus(r.Context(), id, body.Status); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishStage(r.Context(), stageEvent{Kind: "question", QuestionID: id})
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": body.Status})
}

// handleStageStream is the live connection.
//
// Open by MC, speaker and the committee's admin at once; all three want the
// same events. The stream opens with the current state so a client that just
// reconnected is correct immediately, without a separate fetch.
func (s *Server) handleStageStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No streaming support behind this writer; better to say so than to
		// hold a connection open that will never deliver anything.
		s.writeJSON(w, http.StatusInternalServerError, errBody{Error: "streaming tidak tersedia", Code: "no_stream"})
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Nginx and most CDNs buffer responses by default, which would hold every
	// event until the stream closed.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	messages, stop := s.cache.Subscribe(ctx, stageChannel)
	defer stop()

	// Retry tells the browser how long to wait before reconnecting. Short,
	// because a dropped connection during a live session should come back fast.
	if _, err := w.Write([]byte("retry: 2000\n\n")); err != nil {
		return
	}

	if rev, state, err := s.store.StageState(ctx); err == nil {
		if body, err := json.Marshal(stageEvent{Kind: "state", Rev: rev, State: state}); err == nil {
			if !writeSSE(w, body) {
				return
			}
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, open := <-messages:
			if !open {
				return
			}
			if !writeSSE(w, msg) {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// A comment line: ignored by EventSource, enough to keep every
			// proxy in the path from calling the connection idle.
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSE frames one event. Reports false when the client has gone, so the
// caller stops rather than writing into a dead connection.
func writeSSE(w http.ResponseWriter, payload []byte) bool {
	// A newline inside the data would end the event early, so a multi-line
	// payload is split across data: lines as the format requires. Compact JSON
	// has none, but this must not depend on that.
	var b strings.Builder
	b.WriteString("data: ")
	b.Write([]byte(strings.ReplaceAll(string(payload), "\n", "\ndata: ")))
	b.WriteString("\n\n")

	_, err := w.Write([]byte(b.String()))
	return err == nil
}

// ---- what the MC and speaker read on load ----
//
// The stage sides had these compiled into the app: the rundown, who is on
// stage, the questions, the room. That was fine while nothing was behind
// them and wrong the moment the committee could edit any of it -- an MC
// reading a rundown from the bundle is reading last week's plan.

func (s *Server) handleStageRundown(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyAgenda, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		return s.store.Sessions(ctx)
	})
}

// handleStageQuestions is every question asked so far.
//
// Not cached: the MC is the one person who must never see a stale list, and
// this is one small query. New ones arrive over the stream; this is what the
// screen starts from after a reload.
func (s *Server) handleStageQuestions(w http.ResponseWriter, r *http.Request) {
	qs, err := s.store.Questions(r.Context(), "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if qs == nil {
		qs = []model.Question{}
	}
	s.writeJSON(w, http.StatusOK, qs)
}

// handleStageRoom projects the committee's guest list down to what the MC
// actually shows on stage. See model.StageRoom for why it is a projection.
func (s *Server) handleStageRoom(w http.ResponseWriter, r *http.Request) {
	s.cachedJSON(w, r, keyStageRoom, s.cfg.CacheTTL, func(ctx context.Context) (any, error) {
		guests, err := s.store.AdminGuests(ctx)
		if err != nil {
			return nil, err
		}
		perEvent, err := s.store.EventFeedback(ctx)
		if err != nil {
			return nil, err
		}

		room := model.StageRoom{
			Recent:   []model.StageArrival{},
			Rewards:  []model.StageReward{},
			Feedback: map[string]model.StageRating{},
		}
		for _, g := range guests {
			if g.Rsvp == model.RsvpYes {
				room.RsvpYes++
			}
			if g.CheckIn != nil && g.Hadir {
				room.Here++
				room.Recent = append(room.Recent, model.StageArrival{Name: g.Name, At: *g.CheckIn})
			}
			if g.Reward != model.RewardCollecting {
				room.Rewards = append(room.Rewards, model.StageReward{Name: g.Name, Status: g.Reward})
			}
		}
		// Newest arrival first: the MC wants to greet whoever just walked in.
		sort.Slice(room.Recent, func(i, j int) bool { return room.Recent[i].At > room.Recent[j].At })

		for id, rows := range perEvent {
			if len(rows) == 0 {
				continue
			}
			total := 0
			var counts [5]int
			for _, f := range rows {
				total += f.Rating
				if f.Rating >= 1 && f.Rating <= 5 {
					counts[f.Rating-1]++
				}
			}
			room.Feedback[id] = model.StageRating{
				Count:  len(rows),
				Avg:    float64(total) / float64(len(rows)),
				Counts: counts,
			}
		}
		return room, nil
	})
}
