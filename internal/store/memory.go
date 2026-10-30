package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
)

// Memory is the no-database Store.
//
// It is not a mock: it enforces the same authorisation rules and the same
// optimistic locking as Postgres, so the workflow you test against it is the
// workflow you get. What it does not do is survive a restart -- every guest
// gets a fresh invite token when the process boots, which is worth knowing
// before pasting a link into WhatsApp.
type Memory struct {
	mu sync.RWMutex

	guests  []*guestRow
	byID    map[string]*guestRow
	byToken map[string]*guestRow

	sessions      []model.Session
	questions     []model.Question
	announcements []model.Announcement
	people        []model.Person

	// cards[owner][from] = note
	cards map[string]map[string]string

	eventFeedback map[string][]model.EventFeedback
	finalFeedback []model.FinalFeedback

	settings model.Settings

	// photos, newest last; the list is reversed on read.
	photos    []model.Photo
	photoData map[string][]byte
	photoMime map[string]string

	stageRev   int64
	stageState json.RawMessage

	// kenalan holds every "Ajak kenalan", oldest first.
	kenalan []kenalanRow

	// seq backs generated ids, so two questions created in the same
	// millisecond cannot collide.
	seq int64
}

// NewMemory builds the store already seeded with the frontend's sample data.
// NewMemory builds the in-process store. seed false gives an empty one: no
// guests, no questions, no announcements -- the same starting point SEED=false
// gives Postgres, so switching between them does not change what the
// committee sees.
//
// The rundown and the two people on stage are seeded either way. They are the
// shape of the event rather than its data, and an app with no rundown at all
// has screens that cannot render.
func NewMemory(seed bool) (*Memory, error) {
	var rows []*guestRow
	if seed {
		var err error
		if rows, err = buildGuests(); err != nil {
			return nil, err
		}
	}

	m := &Memory{
		guests:        rows,
		byID:          make(map[string]*guestRow, len(rows)),
		byToken:       make(map[string]*guestRow, len(rows)),
		sessions:      seedSessions(),
		people:        seedPeople(),
		cards:         map[string]map[string]string{},
		eventFeedback: map[string][]model.EventFeedback{},
		settings:      defaultSettings(),
		photoData:     map[string][]byte{},
		photoMime:     map[string]string{},
	}
	if seed {
		m.questions = seedQuestions()
		m.announcements = seedAnnouncements()
	}
	for _, g := range rows {
		m.byID[g.ID] = g
		m.byToken[g.InviteToken] = g
	}

	// Materialise the kenalan cards. seedCards also rewrites each guest's
	// count to match the rows it created, so this store reports exactly what
	// Postgres would.
	m.cards = seedCards(rows)
	return m, nil
}

func (m *Memory) Kind() string { return "memory" }
func (m *Memory) Close() error { return nil }

func (m *Memory) nextID(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s%d-%d", prefix, time.Now().UnixMilli(), m.seq)
}

// ---- guests ----

func (m *Memory) SetSprite(ctx context.Context, guestID string, raw []byte, mime string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	g.sprite, g.spriteMime, g.spriteAt = raw, mime, time.Now().UnixMilli()
	return nil
}

func (m *Memory) Sprite(ctx context.Context, guestID string) ([]byte, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.byID[guestID]
	if !ok || g.sprite == nil {
		return nil, "", ErrNotFound
	}
	return g.sprite, g.spriteMime, nil
}

func (m *Memory) PresentGuests(ctx context.Context) ([]model.Guest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.Guest, 0, len(m.guests))
	for _, g := range m.guests {
		// Only guests who finished their character show up as kenalan cards:
		// checked in but still customising would show an empty character.
		if !g.Hadir {
			continue
		}
		out = append(out, model.Guest{
			ID:       g.ID,
			Name:     g.Name,
			Role:     g.profile.Role,
			Question: g.profile.Question,
			Outfit:   g.profile.Outfit,
			Code:     g.profile.Code,
			SpriteAt: g.spriteAt,
		})
	}
	return out, nil
}

func (m *Memory) AdminGuests(ctx context.Context) ([]model.AdminGuest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.AdminGuest, 0, len(m.guests))
	for _, g := range m.guests {
		out = append(out, g.AdminGuest)
	}
	return out, nil
}

func (m *Memory) AddGuest(ctx context.Context, name, phone, email, inviteToken string) (model.AdminGuest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, g := range m.guests {
		if g.Phone == phone {
			return model.AdminGuest{}, fmt.Errorf("%w: %s is already invited", ErrDuplicate, phone)
		}
	}

	id := lower(name)
	// Names repeat in a guest list; ids must not.
	if _, taken := m.byID[id]; taken {
		for n := 2; ; n++ {
			candidate := id + "-" + strconv.Itoa(n)
			if _, taken := m.byID[candidate]; !taken {
				id = candidate
				break
			}
		}
	}
	if email == "" {
		email = id + "@contoh.id"
	}

	row := &guestRow{
		AdminGuest: model.AdminGuest{
			ID:          id,
			Name:        name,
			Email:       email,
			Phone:       phone,
			Rsvp:        model.RsvpUnset,
			Invite:      model.InviteNone,
			Reward:      model.RewardCollecting,
			InviteToken: inviteToken,
		},
		profile: profileFor(id, len(m.guests)),
	}
	m.guests = append(m.guests, row)
	m.byID[id] = row
	m.byToken[inviteToken] = row
	return row.AdminGuest, nil
}

func (m *Memory) GuestByInviteToken(ctx context.Context, tok string) (model.AdminGuest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	g, ok := m.byToken[tok]
	if !ok {
		return model.AdminGuest{}, ErrNotFound
	}
	if g.revoked {
		return model.AdminGuest{}, ErrRevoked
	}
	return g.AdminGuest, nil
}

func (m *Memory) SetRsvp(ctx context.Context, guestID string, r model.Rsvp) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	g.Rsvp = r
	return nil
}

func (m *Memory) CheckIn(ctx context.Context, guestID string, minute int, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	// Checking in twice keeps the first arrival time: that is the one the
	// committee reports on, and a reload should not rewrite it.
	if g.CheckIn == nil {
		g.CheckIn = &minute
		g.CheckInBy = by
	}
	// Arriving implies they are coming, whatever the RSVP said.
	if g.Rsvp != model.RsvpYes {
		g.Rsvp = model.RsvpYes
	}
	return nil
}

func (m *Memory) SetHadir(ctx context.Context, guestID string, hadir bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	g.Hadir = hadir
	return nil
}

func (m *Memory) UndoCheckIn(ctx context.Context, guestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	// "hadir" goes with it: somebody who was never at the door did not finish
	// their character there either, and the kenalan list is built from it.
	g.CheckIn = nil
	g.CheckInBy = ""
	g.Hadir = false
	return nil
}

func (m *Memory) ClaimReward(ctx context.Context, guestID string, minute int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	switch g.Reward {
	case model.RewardClaimed:
		return fmt.Errorf("%w: hadiah sudah diambil", ErrConflict)
	case model.RewardReady:
		g.Reward = model.RewardClaimed
		g.ClaimedAt = &minute
		return nil
	default:
		return fmt.Errorf("%w: kartu kenalan belum lengkap", ErrConflict)
	}
}

// ---- the shared gallery ----

func (m *Memory) Photos(ctx context.Context) ([]model.Photo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Newest first, the order the gallery reads in.
	out := make([]model.Photo, 0, len(m.photos))
	for i := len(m.photos) - 1; i >= 0; i-- {
		out = append(out, m.photos[i])
	}
	return out, nil
}

func (m *Memory) AddPhoto(ctx context.Context, up model.PhotoUpload, limit int) (model.Photo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	mine := 0
	for _, p := range m.photos {
		if p.GuestID == up.GuestID {
			mine++
		}
	}
	if mine >= limit {
		return model.Photo{}, fmt.Errorf("%w: sudah mencapai batas foto", ErrConflict)
	}

	m.seq++
	p := model.Photo{
		ID:      fmt.Sprintf("ph%d", m.seq),
		GuestID: up.GuestID,
		Author:  up.Author,
		RoomID:  up.RoomID,
		Caption: up.Caption,
		At:      up.At,
	}
	m.photos = append(m.photos, p)
	m.photoData[p.ID] = up.Bytes
	m.photoMime[p.ID] = up.Mime
	return p, nil
}

func (m *Memory) DeletePhoto(ctx context.Context, id, guestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, p := range m.photos {
		if p.ID != id {
			continue
		}
		if p.GuestID != guestID {
			return ErrForbidden
		}
		m.photos = append(m.photos[:i], m.photos[i+1:]...)
		delete(m.photoData, id)
		delete(m.photoMime, id)
		return nil
	}
	return ErrNotFound
}

func (m *Memory) PhotoBytes(ctx context.Context, id string) ([]byte, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	b, ok := m.photoData[id]
	if !ok {
		return nil, "", ErrNotFound
	}
	return b, m.photoMime[id], nil
}

func (m *Memory) CensorPhotos(ctx context.Context, ids []string, censored bool, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !censored {
		reason = ""
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	for i := range m.photos {
		if wanted[m.photos[i].ID] {
			m.photos[i].Censored = censored
			m.photos[i].CensorReason = reason
		}
	}
	return nil
}

func (m *Memory) MarkPhotosChecked(ctx context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	for i := range m.photos {
		if wanted[m.photos[i].ID] {
			m.photos[i].Checked = true
		}
	}
	return nil
}

func (m *Memory) Settings(ctx context.Context) (model.Settings, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings, nil
}

func (m *Memory) SaveSettings(ctx context.Context, s model.Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings = s
	return nil
}

func (m *Memory) SetOutfit(ctx context.Context, guestID string, o model.Outfit) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	g.Outfit = o
	// The kenalan card carries a one-line version of the same choice, so it
	// is derived here rather than stored twice and allowed to disagree.
	g.profile.Outfit = o.Label()
	return nil
}

func (m *Memory) MarkInvite(ctx context.Context, guestIDs []string, status model.InviteStatus, errText string, minute int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, id := range guestIDs {
		g, ok := m.byID[id]
		if !ok {
			return fmt.Errorf("%w: guest %s", ErrNotFound, id)
		}
		g.Invite = status
		g.InviteError = errText
		if status == model.InviteSent || status == model.InviteFailed {
			at := minute
			g.InvitedAt = &at
		}
	}
	return nil
}

func (m *Memory) RevokeInvite(ctx context.Context, guestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	g.revoked = true
	return nil
}

// ---- rundown ----

func (m *Memory) Sessions(ctx context.Context) ([]model.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]model.Session(nil), m.sessions...), nil
}

func (m *Memory) SaveSessions(ctx context.Context, s []model.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sort.SliceStable(s, func(i, j int) bool { return s[i].Start < s[j].Start })
	m.sessions = append([]model.Session(nil), s...)
	return nil
}

// ---- questions ----

func (m *Memory) Questions(ctx context.Context, eventID string) ([]model.Question, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]model.Question, 0, len(m.questions))
	for _, q := range m.questions {
		if eventID == "" || q.EventID == eventID {
			out = append(out, q)
		}
	}
	return out, nil
}

func (m *Memory) AddQuestion(ctx context.Context, q model.Question) (model.Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if q.ID == "" {
		q.ID = m.nextID("q")
	}
	if q.Status == "" {
		q.Status = model.QuestionSent
	}
	m.questions = append(m.questions, q)
	return q, nil
}

func (m *Memory) EditQuestion(ctx context.Context, id, guestID, text string) (model.Question, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.questions {
		if m.questions[i].ID != id {
			continue
		}
		if m.questions[i].AuthorGuestID != guestID {
			return model.Question{}, ErrForbidden
		}
		// Once the MC has read it, editing would change the question under
		// them mid-sentence. The frontend has a review case for exactly this
		// race (askRace in src/guest/state.ts).
		if m.questions[i].Status != model.QuestionSent {
			return model.Question{}, fmt.Errorf("%w: question already read by the MC", ErrConflict)
		}
		m.questions[i].Text = text
		m.questions[i].Edited = true
		return m.questions[i], nil
	}
	return model.Question{}, ErrNotFound
}

func (m *Memory) DeleteQuestion(ctx context.Context, id, guestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.questions {
		if m.questions[i].ID != id {
			continue
		}
		if m.questions[i].AuthorGuestID != guestID {
			return ErrForbidden
		}
		m.questions = append(m.questions[:i], m.questions[i+1:]...)
		return nil
	}
	return ErrNotFound
}

func (m *Memory) SetQuestionStatus(ctx context.Context, id string, s model.QuestionStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.questions {
		if m.questions[i].ID == id {
			m.questions[i].Status = s
			return nil
		}
	}
	return ErrNotFound
}

// ---- kenalan cards ----

func (m *Memory) AddCard(ctx context.Context, ownerID, fromID, note string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	owner, ok := m.byID[ownerID]
	if !ok {
		return 0, ErrNotFound
	}
	if _, ok := m.byID[fromID]; !ok {
		return 0, fmt.Errorf("%w: guest %s", ErrNotFound, fromID)
	}
	if ownerID == fromID {
		return owner.Cards, fmt.Errorf("%w: cannot collect your own card", ErrForbidden)
	}

	if m.cards[ownerID] == nil {
		m.cards[ownerID] = map[string]string{}
	}
	// Collecting the same card twice must not advance the prize counter --
	// this is the write most likely to be replayed from an offline queue.
	if _, already := m.cards[ownerID][fromID]; already {
		return owner.Cards, nil
	}
	m.cards[ownerID][fromID] = note

	owner.Cards = len(m.cards[ownerID])
	if owner.Cards >= 5 && owner.Reward == model.RewardCollecting {
		owner.Reward = model.RewardReady
	}
	return owner.Cards, nil
}

func (m *Memory) Cards(ctx context.Context, ownerID string) ([]model.Card, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	held := m.cards[ownerID]
	out := make([]model.Card, 0, len(held))
	for from, note := range held {
		out = append(out, model.Card{GuestID: from, Note: note})
	}
	// Map order is random in Go; a stable list keeps the UI from reshuffling.
	sort.Slice(out, func(i, j int) bool { return out[i].GuestID < out[j].GuestID })
	return out, nil
}

// ---- feedback ----

func (m *Memory) AddEventFeedback(ctx context.Context, guestID string, f model.EventFeedback) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.byID[guestID]; !ok {
		return ErrNotFound
	}
	// One rating per guest per session: a replayed write updates rather than
	// stacking a second vote.
	list := m.eventFeedback[f.EventID]
	for i := range list {
		if list[i].GuestID == guestID {
			list[i].Rating = f.Rating
			list[i].Text = f.Text
			list[i].At = f.At
			m.eventFeedback[f.EventID] = list
			return nil
		}
	}
	f.GuestID = guestID
	m.eventFeedback[f.EventID] = append(list, f)
	return nil
}

func (m *Memory) AddFinalFeedback(ctx context.Context, guestID string, f model.FinalFeedback) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.byID[guestID]
	if !ok {
		return ErrNotFound
	}
	for i := range m.finalFeedback {
		if m.finalFeedback[i].GuestID == guestID {
			f.GuestID = guestID
			m.finalFeedback[i] = f
			return nil
		}
	}
	f.GuestID = guestID
	m.finalFeedback = append(m.finalFeedback, f)
	g.FinalFeedback = true
	return nil
}

func (m *Memory) EventFeedback(ctx context.Context) (map[string][]model.EventFeedback, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[string][]model.EventFeedback, len(m.eventFeedback))
	for k, v := range m.eventFeedback {
		out[k] = append([]model.EventFeedback(nil), v...)
	}
	return out, nil
}

func (m *Memory) RatedEvents(ctx context.Context, guestID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := []string{}
	for id, list := range m.eventFeedback {
		for _, f := range list {
			if f.GuestID == guestID {
				out = append(out, id)
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) FinalFeedback(ctx context.Context) ([]model.FinalFeedback, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]model.FinalFeedback(nil), m.finalFeedback...), nil
}

// ---- announcements ----

func (m *Memory) Announcements(ctx context.Context) ([]model.Announcement, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]model.Announcement(nil), m.announcements...), nil
}

func (m *Memory) AddAnnouncement(ctx context.Context, a model.Announcement) (model.Announcement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if a.ID == "" {
		a.ID = m.nextID("a")
	}
	if a.Recipients == 0 {
		// Everybody who is actually in the building.
		for _, g := range m.guests {
			if g.CheckIn != nil {
				a.Recipients++
			}
		}
	}
	if a.Source == "" {
		a.Source = "panitia"
	}
	m.announcements = append(m.announcements, a)
	return a, nil
}

func (m *Memory) DeleteAnnouncement(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.announcements {
		if m.announcements[i].ID == id {
			m.announcements = append(m.announcements[:i], m.announcements[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

type kenalanRow struct {
	inv model.KenalanInvite
	at  time.Time
}

func (m *Memory) AddKenalanInvite(ctx context.Context, fromID, toID string, minute int, cooldown time.Duration) (model.KenalanInvite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, ok := m.byID[fromID]
	to, ok2 := m.byID[toID]
	if !ok || !ok2 || !to.Hadir {
		return model.KenalanInvite{}, ErrNotFound
	}
	now := time.Now()
	for _, k := range m.kenalan {
		if k.inv.FromID == fromID && k.inv.ToID == toID && now.Sub(k.at) < cooldown {
			return model.KenalanInvite{}, fmt.Errorf("%w: kamu baru saja mengajaknya, coba lagi nanti", ErrConflict)
		}
	}
	inv := model.KenalanInvite{ID: m.nextID("k"), FromID: fromID, FromName: from.Name, ToID: toID, At: minute, SentAt: now.UnixMilli()}
	m.kenalan = append(m.kenalan, kenalanRow{inv: inv, at: now})
	return inv, nil
}

func (m *Memory) KenalanInvites(ctx context.Context, guestID string) ([]model.KenalanInvite, []model.KenalanInvite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	received := []model.KenalanInvite{}
	latest := map[string]model.KenalanInvite{}
	for i := len(m.kenalan) - 1; i >= 0; i-- {
		k := m.kenalan[i].inv
		if k.ToID == guestID && len(received) < 50 {
			received = append(received, k)
		}
		if _, seen := latest[k.ToID]; k.FromID == guestID && !seen {
			latest[k.ToID] = k
		}
	}
	sent := make([]model.KenalanInvite, 0, len(latest))
	for _, k := range latest {
		sent = append(sent, k)
	}
	return received, sent, nil
}

func (m *Memory) ResendAnnouncement(ctx context.Context, id string, minute int) (model.Announcement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.announcements {
		if m.announcements[i].ID == id {
			a := &m.announcements[i]
			a.At, a.Failed, a.Recipients = minute, false, 0
			for _, g := range m.guests {
				if g.CheckIn != nil {
					a.Recipients++
				}
			}
			return *a, nil
		}
	}
	return model.Announcement{}, ErrNotFound
}

// ---- people ----

func (m *Memory) People(ctx context.Context) ([]model.Person, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]model.Person(nil), m.people...)
	for i := range out {
		if out[i].Access == "" {
			out[i].Access = "belum"
		}
	}
	return out, nil
}

// setAccess moves one person's link status. next decides from the current
// status, so "terkirim" never overwrites "masuk".
func (m *Memory) setAccess(id string, next func(cur string) string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.people {
		if m.people[i].ID == id {
			m.people[i].Access = next(m.people[i].Access)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) MarkPersonInvited(ctx context.Context, id string) error {
	return m.setAccess(id, func(cur string) string {
		if cur == "" || cur == "belum" {
			return "terkirim"
		}
		return cur
	})
}

func (m *Memory) MarkPersonLogin(ctx context.Context, id string) error {
	return m.setAccess(id, func(cur string) string {
		if cur == "dicabut" {
			return cur
		}
		return "masuk"
	})
}

func (m *Memory) SetPersonRevoked(ctx context.Context, id string, revoked bool) error {
	return m.setAccess(id, func(cur string) string {
		if revoked {
			return "dicabut"
		}
		return "terkirim"
	})
}

func (m *Memory) SavePerson(ctx context.Context, p model.Person) (model.Person, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if p.ID == "" {
		p.ID = newID("p")
		p.Access = "belum"
		p.StageCode = NewStageCode()
		m.people = append(m.people, p)
		return p, nil
	}
	for i := range m.people {
		if m.people[i].ID == p.ID {
			// Access is the store's to derive, never the caller's to set,
			// and the code stays what it was.
			p.Access = m.people[i].Access
			p.StageCode = m.people[i].StageCode
			m.people[i] = p
			return p, nil
		}
	}
	return p, ErrNotFound
}

func (m *Memory) DeletePerson(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.people {
		if m.people[i].ID == id {
			m.people = append(m.people[:i], m.people[i+1:]...)
			for j := range m.sessions {
				if m.sessions[j].SpeakerID == id {
					m.sessions[j].SpeakerID = ""
				}
			}
			return nil
		}
	}
	return ErrNotFound
}

// ---- stage ----

func (m *Memory) StageState(ctx context.Context) (int64, json.RawMessage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stageRev, m.stageState, nil
}

func (m *Memory) SaveStageState(ctx context.Context, rev int64, state json.RawMessage) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// rev -1 is "overwrite whatever is there", which is what a review case
	// seeding the stage does on purpose.
	if rev >= 0 && rev != m.stageRev {
		return m.stageRev, ErrConflict
	}
	m.stageRev++
	m.stageState = append(json.RawMessage(nil), state...)
	return m.stageRev, nil
}

// ---- dashboard ----

func (m *Memory) Dashboard(ctx context.Context) (model.Dashboard, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	d := model.Dashboard{RatingCounts: map[string]int{}}
	for _, g := range m.guests {
		d.Invited++
		switch g.Rsvp {
		case model.RsvpYes:
			d.RsvpYes++
		case model.RsvpNo:
			d.RsvpNo++
		default:
			d.RsvpPending++
		}
		if g.CheckIn != nil {
			d.CheckedIn++
		}
		if g.Hadir {
			d.Hadir++
		}
		switch g.Reward {
		case model.RewardReady:
			d.RewardsReady++
		case model.RewardClaimed:
			d.RewardsTaken++
		}
	}
	d.Questions = len(m.questions)
	d.FinalFeedback = len(m.finalFeedback)
	for _, f := range m.finalFeedback {
		d.RatingCounts[strconv.Itoa(f.Rating)]++
	}
	return d, nil
}
