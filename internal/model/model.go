// Package model holds the wire types.
//
// Every JSON tag matches the TypeScript field it feeds, so a response drops
// straight into the frontend's existing types with no mapping layer:
//
//	Guest              -> src/data/guests.ts
//	AgendaItem/Session -> src/data/agenda.ts, src/stage/data.ts
//	Question           -> src/data/questions.ts  (AskedQuestion)
//	AdminGuest         -> src/admin/data.ts
//
// Clock fields called `at`, `start`, `end` and `checkIn` are *rundown minutes*
// (minutes since midnight), not unix time -- that is the unit the whole app
// already thinks in. Real wall-clock instants are named explicitly and are
// unix milliseconds, to match Date.now() on the client.
package model

// Rsvp mirrors src/admin/data.ts.
type Rsvp string

const (
	RsvpYes   Rsvp = "ya"
	RsvpNo    Rsvp = "tidak"
	RsvpUnset Rsvp = "belum"
)

// InviteStatus is the delivery state of a guest's WhatsApp invitation.
type InviteStatus string

const (
	InviteNone    InviteStatus = "belum"
	InviteSending InviteStatus = "mengirim"
	InviteSent    InviteStatus = "terkirim"
	InviteRead    InviteStatus = "dibaca"
	InviteFailed  InviteStatus = "gagal"
)

// RewardStatus is the card-collecting prize state.
type RewardStatus string

const (
	RewardCollecting RewardStatus = "mengumpulkan"
	RewardReady      RewardStatus = "siap"
	RewardClaimed    RewardStatus = "diambil"
)

// QuestionStatus mirrors src/data/questions.ts.
type QuestionStatus string

const (
	QuestionSent     QuestionStatus = "terkirim"
	QuestionRead     QuestionStatus = "dibaca"
	QuestionAnswered QuestionStatus = "terjawab"
)

// Place is where the event happens, as the committee types it in.
type Place struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// Settings is the Pengaturan page, and the source of truth for the two codes
// the door uses.
//
// DoorCode and PanitiaPIN start from the environment and are then owned by
// this row: once the committee changes a code on screen, the environment's
// copy is history. That is the point of the page -- a code the committee can
// rotate during the event without a redeploy.
type Settings struct {
	EventName string `json:"eventName"`
	// Date is "2026-11-08": a calendar day, with no time and no zone, because
	// that is what it means. Sending it as a timestamp invites a browser in
	// another zone to render the day before.
	Date  string `json:"date"`
	Place Place  `json:"place"`

	DoorCode   string `json:"doorCode"`
	PanitiaPIN string `json:"panitiaPin"`

	Countdown bool `json:"countdown"`

	// Rundown minutes, like everything else on the clock in this app.
	FinalFeedbackAt int `json:"finalFeedbackAt"`
	CodesChangedAt  int `json:"codesChangedAt"`

	// InviteTemplate is the WhatsApp invitation, with {nama} and {link}.
	// Empty means the built-in one.
	InviteTemplate string `json:"inviteTemplate"`
	// Contact is who guests, the MC and speakers call for help.
	Contact Contact `json:"contact"`
}

// Contact is the committee member on call. Both empty means nobody is set,
// and the screens hide the button rather than dialling nobody.
type Contact struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

// Outfit is what a guest dressed their character in: one label per category,
// matching Outfit in src/data/outfits.ts. Empty string means that slot is
// bare, which is a real choice -- a guest can wear no accessory.
//
// Labels, not Spline node names. The node names belong to whichever export of
// the scene is current and change when the scene is re-exported; the labels
// are what the guest picked and what a kenalan card reads out, so they are
// what survives in the database.
type Outfit struct {
	Baju      string `json:"baju"`
	Celana    string `json:"celana"`
	Aksesoris string `json:"aksesoris"`
}

// The wearable labels, mirroring OUTFIT_CATEGORIES in src/data/outfits.ts.
//
// Worth checking against, even though only an authenticated guest can write
// one: an outfit label is shown to every other phone in the room on a kenalan
// card. Unchecked, the field is a free-text broadcast to the whole event. An
// allowlist also catches the real mistake -- a stale app sending a label from
// an older catalogue -- loudly, instead of storing something no character can
// actually wear.
var outfitChoices = map[string]map[string]bool{
	"baju": {
		"Hoodie": true, "Kaos bintang": true, "Kemeja pita": true,
		"Sweater vest": true, "Jaket varsity": true,
	},
	"celana": {
		"Celana hitam": true, "Celana khaki": true, "Overall denim": true,
		"Rok krem": true, "Rok kotak": true,
	},
	"aksesoris": {
		"Topi navy": true, "Bucket hat": true, "Topi jerami": true,
		"Beret hitam": true, "Beret abu": true, "Kacamata aviator": true,
		"Kacamata oranye": true, "Cat eye pink": true, "Jepit bunga": true,
	},
}

// Valid reports whether every filled slot holds a real item from its own
// category. Empty is allowed everywhere: a guest may wear no accessory, and a
// guest who wears nothing at all is still a guest.
func (o Outfit) Valid() (string, bool) {
	for cat, picked := range map[string]string{
		"baju": o.Baju, "celana": o.Celana, "aksesoris": o.Aksesoris,
	} {
		if picked != "" && !outfitChoices[cat][picked] {
			return cat, false
		}
	}
	return "", true
}

// Label is the one-line summary shown on a kenalan card. The top is what
// reads best there -- "Hoodie" identifies someone across a room; "Hoodie,
// celana khaki, kacamata" does not.
func (o Outfit) Label() string {
	if o.Baju != "" {
		return o.Baju
	}
	if o.Celana != "" {
		return o.Celana
	}
	return o.Aksesoris
}

// Guest is one guest as another guest sees them: the kenalan card. It carries
// no contact details on purpose -- this shape is served to every phone in the
// room, so a phone number here would be a leak.
type Guest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Question string `json:"question"`
	Outfit   string `json:"outfit"`
	Code     string `json:"code"`
	// SpriteAt is when their character picture was last uploaded, as Unix
	// milliseconds; 0 means none yet. It versions the image URL.
	SpriteAt int64 `json:"spriteAt,omitempty"`
}

// AdminGuest is the committee's view: everything, contact details included.
// Never served to a guest token.
type AdminGuest struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
	Rsvp  Rsvp   `json:"rsvp"`

	// CheckIn is the rundown minute they arrived; nil means not here yet.
	CheckIn   *int   `json:"checkIn"`
	CheckInBy string `json:"checkInBy,omitempty"`
	// Hadir is checked in AND finished their character. Checked in only shows
	// as "Sedang masuk" in the admin.
	Hadir bool `json:"hadir"`

	Invite      InviteStatus `json:"invite"`
	InvitedAt   *int         `json:"invitedAt,omitempty"`
	InviteError string       `json:"inviteError,omitempty"`

	Cards         int          `json:"cards"`
	Reward        RewardStatus `json:"reward"`
	ClaimedAt     *int         `json:"claimedAt,omitempty"`
	FinalFeedback bool         `json:"finalFeedback"`

	// Outfit is what they dressed their character in, empty until they finish
	// the customisation screen. The committee sees it because "who has built a
	// character" is the same question as "who is really in the room".
	Outfit Outfit `json:"outfit"`

	// InviteToken is this guest's personal link. Admin-only: it is the
	// credential, so it is never included in a guest-facing response.
	InviteToken string `json:"inviteToken,omitempty"`
	// InviteURL is InviteToken rendered against APP_BASE_URL, ready to paste
	// into WhatsApp.
	InviteURL string `json:"inviteUrl,omitempty"`
}

// AgendaItem is one rundown row.
type AgendaItem struct {
	ID     string `json:"id"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Title  string `json:"title"`
	RoomID string `json:"roomId"`
}

// Session adds who runs the item and whether guests may send questions.
type Session struct {
	AgendaItem
	PIC       string `json:"pic,omitempty"`
	SpeakerID string `json:"speakerId,omitempty"`
	QnA       bool   `json:"qna"`
}

// Person is an MC or a speaker.
type Person struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  string `json:"role"` // "mc" | "pembicara"
	Title string `json:"title"`
	Phone string `json:"phone"`
	// Access is where their personal link stands, for the Pengisi page:
	// "belum" (not sent), "terkirim", "masuk" (has signed in) or "dicabut".
	// Derived by the store; ignored on writes.
	Access string `json:"access"`
}

// Question is one question sent to the stage.
type Question struct {
	ID      string `json:"id"`
	EventID string `json:"eventId"`
	Text    string `json:"text"`
	// Author is nil when sent anonymously.
	Author *string `json:"author"`
	// Mine is computed per request from the caller's token, never stored.
	Mine   bool           `json:"mine"`
	Status QuestionStatus `json:"status"`
	At     int            `json:"at"`
	Edited bool           `json:"edited,omitempty"`

	// AuthorGuestID is who actually sent it. Internal: it decides `Mine` and
	// who may edit or delete, and is dropped from guest-facing JSON.
	AuthorGuestID string `json:"-"`
}

// Announcement is a push from the committee or the MC.
type Announcement struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Body       string  `json:"body"`
	RoomID     *string `json:"roomId"`
	At         int     `json:"at"`
	Recipients int     `json:"recipients"`
	Read       int     `json:"read"`
	Failed     bool    `json:"failed,omitempty"`
	// Source is "panitia" or "mc": who announced it.
	Source string `json:"source"`
}

// KenalanInvite is one guest letting another know they would like to meet.
type KenalanInvite struct {
	ID       string `json:"id"`
	FromID   string `json:"fromId"`
	FromName string `json:"fromName"`
	ToID     string `json:"toId"`
	// At is the rundown minute it was sent, like everything else on screen.
	At int `json:"at"`
	// SentAt is Unix milliseconds, for the sender's "Ajak lagi" countdown.
	SentAt int64 `json:"sentAt"`
}

// Card is a kenalan card one guest collected from another.
type Card struct {
	GuestID string `json:"guestId"`
	Note    string `json:"note"`
}

// EventFeedback is one rating for one rundown item.
type EventFeedback struct {
	EventID string `json:"eventId"`
	Rating  int    `json:"rating"`
	Text    string `json:"text,omitempty"`
	At      int    `json:"at"`

	// GuestID is who sent it. Internal: it enforces one rating per guest per
	// session, and the committee reads feedback in aggregate, never by name.
	GuestID string `json:"-"`
}

// FinalFeedback is the closing survey.
type FinalFeedback struct {
	Rating int      `json:"rating"`
	Note   string   `json:"note,omitempty"`
	Moods  []string `json:"moods,omitempty"`
	At     int      `json:"at"`

	// GuestID is who sent it, kept out of JSON for the same reason.
	GuestID string `json:"-"`
}

// Invite is what a guest's own link resolves to. This is the first thing a
// guest's phone ever loads, so it carries exactly what the RSVP screen needs
// and nothing more.
type Invite struct {
	GuestID string `json:"guestId"`
	Name    string `json:"name"`
	Rsvp    Rsvp   `json:"rsvp"`
	// CheckedIn lets a returning guest skip straight past the door.
	CheckedIn bool `json:"checkedIn"`
	Hadir     bool `json:"hadir"`
	// Code is this guest's own kenalan code, the one other guests type in.
	Code string `json:"code"`
	// Cards is how many they have collected so far.
	Cards int `json:"cards"`
	// RewardClaimed and FinalFeedback are what the phone used to remember on
	// its own; from the server they survive a second phone or cleared storage.
	RewardClaimed bool `json:"rewardClaimed"`
	FinalFeedback bool `json:"finalFeedback"`
	// RatedEvents is the sessions this guest already rated, so the "beri
	// rating" prompt does not come back on a second phone.
	RatedEvents []string `json:"ratedEvents"`
	// Outfit is the character they already built, so a guest who reopens their
	// link on a different phone gets their own character back rather than an
	// empty customisation screen. This is the whole reason it is stored
	// server-side instead of in localStorage.
	Outfit Outfit `json:"outfit"`

	// Countdown is the committee's switch for the pre-event screen. It comes
	// from the settings row rather than the app's bundle, so turning it off
	// on the Pengaturan page reaches guests' phones instead of needing a
	// rebuild and a redeploy.
	Countdown bool `json:"countdown"`

	Event EventInfo `json:"event"`
}

// EventInfo mirrors EVENT in src/config.ts, so the client can stop hardcoding
// it once the backend is in place.
type EventInfo struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	DateLabel string `json:"dateLabel"`
	Place     string `json:"place"`
	StartsAt  string `json:"startsAt"`
	// Contact is public on purpose: it is the number on the "Hubungi
	// panitia" button, which guests see anyway.
	Contact Contact `json:"contact"`
}

// Dashboard is the admin landing aggregate, computed server-side so the
// committee's laptop does not pull every guest row to count them.
type Dashboard struct {
	Invited       int            `json:"invited"`
	RsvpYes       int            `json:"rsvpYes"`
	RsvpNo        int            `json:"rsvpNo"`
	RsvpPending   int            `json:"rsvpPending"`
	CheckedIn     int            `json:"checkedIn"`
	Hadir         int            `json:"hadir"`
	RewardsReady  int            `json:"rewardsReady"`
	RewardsTaken  int            `json:"rewardsTaken"`
	Questions     int            `json:"questions"`
	FinalFeedback int            `json:"finalFeedback"`
	RatingCounts  map[string]int `json:"ratingCounts"`
}

// StageRoom is what the MC's "sorotan" tab shows about the room.
//
// A deliberate projection, not AdminGuest with fields dropped: the MC is
// signed in with a six-digit code shared around a stage, and the committee's
// list carries phone numbers and every guest's invite token. Nothing here
// could hurt if the MC's phone were left unlocked on a table.
type StageRoom struct {
	Here    int `json:"here"`
	RsvpYes int `json:"rsvpYes"`

	// Recent is who walked in last, newest first.
	Recent []StageArrival `json:"recent"`
	// Rewards is who has earned or collected a prize.
	Rewards []StageReward `json:"rewards"`
	// Feedback is the average rating per session, for the sessions that have
	// any. Keyed by session id.
	Feedback map[string]StageRating `json:"feedback"`
}

type StageArrival struct {
	Name string `json:"name"`
	// At is a rundown minute.
	At int `json:"at"`
}

type StageReward struct {
	Name   string       `json:"name"`
	Status RewardStatus `json:"status"`
}

type StageRating struct {
	Count int     `json:"count"`
	Avg   float64 `json:"avg"`
	// Counts is how many gave 1, 2, 3, 4 and 5 stars, in that order. The MC
	// screen draws the distribution, and an average alone hides the case
	// worth knowing about: a 4.0 made of fives and ones.
	Counts [5]int `json:"counts"`
}

// Photo is one picture in the shared gallery, without its bytes.
//
// The image is fetched separately (GET /api/photos/{id}/image) so a list of
// forty tiles is one small JSON response and forty cacheable image requests,
// rather than one enormous payload the phone cannot cache per photo.
type Photo struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	RoomID  string `json:"roomId"`
	Caption string `json:"caption,omitempty"`
	// At is a rundown minute.
	At int `json:"at"`
	// Mine is computed per request from the caller's token, never stored.
	Mine bool `json:"mine"`
	// Censored means other guests get a thumbnail too small to read.
	Censored bool `json:"censored,omitempty"`

	// GuestID decides Mine and who may delete this; never sent to a guest.
	GuestID string `json:"-"`

	// CensorReason and Checked are the committee's own notes. They are set
	// only on the admin endpoint; the guest list leaves them empty.
	CensorReason string `json:"censorReason,omitempty"`
	Checked      bool   `json:"checked,omitempty"`
}

// PhotoUpload is one resized image on its way in.
type PhotoUpload struct {
	GuestID string
	Author  string
	RoomID  string
	Caption string
	At      int
	Mime    string
	Bytes   []byte
}
