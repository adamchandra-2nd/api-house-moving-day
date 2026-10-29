// Package store is the data layer: one interface, two implementations.
//
// Memory is the default and needs nothing installed, so the whole invite and
// check-in workflow is testable before signing up for any service. Postgres
// talks to Supabase and is what production uses. Handlers only ever see this
// interface, so swapping one for the other changes no HTTP code.
package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
)

var (
	// ErrNotFound is a missing row: an unknown invite token, question or guest.
	ErrNotFound = errors.New("not found")
	// ErrForbidden is a row that exists but does not belong to this caller,
	// e.g. editing somebody else's question.
	ErrForbidden = errors.New("forbidden")
	// ErrConflict is a lost optimistic-locking race on the stage state.
	ErrConflict = errors.New("conflict")
	// ErrRevoked is a token the committee has turned off.
	ErrRevoked = errors.New("invite revoked")
	// ErrDuplicate is a guest whose phone number is already on the list.
	ErrDuplicate = errors.New("duplicate")
)

// Store is everything the API needs from a database.
type Store interface {
	// Kind is "memory" or "postgres", for the boot log and /healthz.
	Kind() string
	Close() error

	// ---- guests, as other guests see them ----

	// PresentGuests is the kenalan list: guests who are actually here. It
	// carries no contact details, because this response goes to every phone.
	PresentGuests(ctx context.Context) ([]model.Guest, error)

	// ---- guests, as the committee sees them ----

	AdminGuests(ctx context.Context) ([]model.AdminGuest, error)
	AddGuest(ctx context.Context, name, phone, email, inviteToken string) (model.AdminGuest, error)

	// GuestByInviteToken resolves a personal link. Returns ErrRevoked for a
	// token the committee has turned off, so the guest gets a clear message
	// instead of "not found".
	GuestByInviteToken(ctx context.Context, token string) (model.AdminGuest, error)

	SetRsvp(ctx context.Context, guestID string, r model.Rsvp) error

	// CheckIn records arrival at the door. minute is a rundown minute.
	CheckIn(ctx context.Context, guestID string, minute int, by string) error

	// UndoCheckIn puts a guest back to "belum datang". The committee needs it
	// because the common mistake at a door is checking in the wrong name, and
	// the only thing worse than that is not being able to take it back.
	UndoCheckIn(ctx context.Context, guestID string) error

	// ClaimReward records that a guest collected their prize. Returns
	// ErrConflict when they were not eligible, so two committee phones
	// handing out the same prize cannot both succeed.
	ClaimReward(ctx context.Context, guestID string, minute int) error

	// SetHadir flips "Sedang masuk" to "Hadir", which the app does once the
	// guest has finished building their character.
	SetHadir(ctx context.Context, guestID string, hadir bool) error

	// SetOutfit saves the character a guest built. Stored against the guest
	// rather than the browser, so reopening the invite link anywhere brings
	// the same character back.
	SetOutfit(ctx context.Context, guestID string, o model.Outfit) error

	// SetSprite stores the picture of a guest's character, which their own
	// phone renders from the 3D scene; Sprite reads it back, ErrNotFound when
	// there is none. Other guests see it on the kenalan cards.
	SetSprite(ctx context.Context, guestID string, raw []byte, mime string) error
	Sprite(ctx context.Context, guestID string) ([]byte, string, error)

	// MarkInvite records a delivery attempt against several guests at once.
	MarkInvite(ctx context.Context, guestIDs []string, status model.InviteStatus, errText string, minute int) error

	// RevokeInvite kills a link without deleting the guest.
	RevokeInvite(ctx context.Context, guestID string) error

	// ---- rundown ----

	Sessions(ctx context.Context) ([]model.Session, error)
	SaveSessions(ctx context.Context, s []model.Session) error

	// ---- questions to the stage ----

	// Questions returns every question, newest last. eventID "" means all.
	Questions(ctx context.Context, eventID string) ([]model.Question, error)
	AddQuestion(ctx context.Context, q model.Question) (model.Question, error)

	// EditQuestion and DeleteQuestion take the caller's guest id and return
	// ErrForbidden when it is not their question. Authorisation lives here, so
	// no handler can forget it.
	EditQuestion(ctx context.Context, id, guestID, text string) (model.Question, error)
	DeleteQuestion(ctx context.Context, id, guestID string) error

	// SetQuestionStatus is the MC marking a question read or answered.
	SetQuestionStatus(ctx context.Context, id string, s model.QuestionStatus) error

	// ---- kenalan cards ----

	// AddCard records that ownerID collected fromID's card, and returns the
	// owner's new total so the client can show progress straight away.
	AddCard(ctx context.Context, ownerID, fromID, note string) (int, error)
	Cards(ctx context.Context, ownerID string) ([]model.Card, error)

	// ---- feedback ----

	AddEventFeedback(ctx context.Context, guestID string, f model.EventFeedback) error
	AddFinalFeedback(ctx context.Context, guestID string, f model.FinalFeedback) error
	EventFeedback(ctx context.Context) (map[string][]model.EventFeedback, error)
	// RatedEvents is the session ids one guest has rated.
	RatedEvents(ctx context.Context, guestID string) ([]string, error)
	FinalFeedback(ctx context.Context) ([]model.FinalFeedback, error)

	// ---- announcements ----

	Announcements(ctx context.Context) ([]model.Announcement, error)
	AddAnnouncement(ctx context.Context, a model.Announcement) (model.Announcement, error)

	// ResendAnnouncement moves an announcement to minute, which is what makes
	// guests' phones show it again as new.
	ResendAnnouncement(ctx context.Context, id string, minute int) (model.Announcement, error)

	// ---- people on stage ----

	People(ctx context.Context) ([]model.Person, error)

	// SavePerson adds a person when ID is empty -- minting a random id, since
	// the id is what the personal stage link carries -- and replaces one
	// otherwise. Returns ErrNotFound for an id that does not exist.
	SavePerson(ctx context.Context, p model.Person) (model.Person, error)

	// DeletePerson removes someone from the cast and unassigns them from any
	// session they were speaking in, so the rundown never points at nobody.
	DeletePerson(ctx context.Context, id string) error

	// MarkPersonInvited records that their personal link went out, and
	// MarkPersonLogin that they signed in with it. Both feed Person.Access.
	MarkPersonInvited(ctx context.Context, id string) error
	MarkPersonLogin(ctx context.Context, id string) error

	// SetPersonRevoked turns a personal link off (or back on). Revoked people
	// cannot sign in, and sessions they already hold stop working.
	SetPersonRevoked(ctx context.Context, id string, revoked bool) error

	// ---- the shared gallery ----

	// Photos is every photo, newest first. The caller decides what to do with
	// GuestID; this does not know who is asking.
	Photos(ctx context.Context) ([]model.Photo, error)

	// AddPhoto refuses past `limit` photos for one guest and returns
	// ErrConflict, so the cap is enforced where the count is, not in a
	// handler that would have to read-then-write.
	AddPhoto(ctx context.Context, p model.PhotoUpload, limit int) (model.Photo, error)

	// DeletePhoto takes the caller's guest id and returns ErrForbidden when
	// the photo is somebody else's. Authorisation lives here, as it does for
	// questions.
	DeletePhoto(ctx context.Context, id, guestID string) error

	// PhotoBytes returns the stored image and its type.
	PhotoBytes(ctx context.Context, id string) ([]byte, string, error)

	// CensorPhotos blurs a batch; reason is the committee's own note. Passing
	// censored false lifts it, which is what "urungkan" does.
	CensorPhotos(ctx context.Context, ids []string, censored bool, reason string) error

	// MarkPhotosChecked records that the committee has looked at these.
	MarkPhotosChecked(ctx context.Context, ids []string) error

	// ---- committee settings ----

	// Settings is one row. SaveSettings replaces it wholesale, because the
	// page saves as a unit and a partial update would let two committee
	// members' edits interleave into a row neither of them chose.
	Settings(ctx context.Context) (model.Settings, error)
	SaveSettings(ctx context.Context, s model.Settings) error

	// ---- shared MC/speaker state ----

	// StageState is stored as an opaque document on purpose. Its shape is the
	// frontend's business (see src/stage/store.ts) and it changes as the stage
	// sides grow; the server's job is to hold the latest one and fan it out,
	// not to re-declare every field and then drift from it.
	StageState(ctx context.Context) (int64, json.RawMessage, error)

	// SaveStageState is optimistically locked: pass the rev that was read, and
	// get ErrConflict if somebody else wrote in between. Two phones editing
	// the same live state is the normal case here, not the exception.
	SaveStageState(ctx context.Context, rev int64, state json.RawMessage) (int64, error)

	// Dashboard is the admin aggregate. It is on the store rather than
	// computed in a handler so Postgres can count in SQL instead of shipping
	// every guest row to the API to be counted in a loop.
	Dashboard(ctx context.Context) (model.Dashboard, error)
}

// Both implementations are checked against the interface here rather than
// only where main wires one up. Adding a method to Store should fail the
// build of this package, next to the declaration, instead of surfacing as a
// confusing error in cmd/server.
var (
	_ Store = (*Memory)(nil)
	_ Store = (*Postgres)(nil)
)
