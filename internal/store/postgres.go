package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/adamchandra-2nd/api-house-moving-day/internal/model"
)

// Postgres is the Supabase-backed Store.
type Postgres struct{ pool *pgxpool.Pool }

// OpenPostgres connects and verifies the connection before returning, so a
// wrong password fails at boot rather than on the first guest's request.
//
// Pool size is deliberately small. Supabase's free tier allows few direct
// connections, and the committee plus a room full of phones all go through the
// pooled port anyway; a handful of connections serving sub-millisecond queries
// is plenty, and asking for more is how you get "too many clients".
func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(sanitizeDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	// Supabase's transaction pooler (port 6543) hands each message to whichever
	// backend is free, so even describe-then-execute breaks: under concurrent
	// requests the describe and the bind land on different backends and fail
	// with "bind message has N result formats but query has M columns". The
	// simple protocol sends one self-contained message per query, which works
	// on the pooled and the direct port alike.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	ping, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(ping); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Kind() string { return "postgres" }
func (p *Postgres) Close() error { p.pool.Close(); return nil }

// clientOnlyParams are query parameters that belong to a specific client
// library rather than to Postgres.
//
// Supabase's "Connect" dialog shows its connection string under an ORM tab,
// and the Prisma one carries ?pgbouncer=true. Prisma reads that flag itself
// and strips it; pgx does not know it, so it forwards it to the server as a
// startup parameter and the connection dies with
//
//	FATAL: unrecognized configuration parameter "pgbouncer"
//
// Since that string is what the dashboard hands people, pasting it in should
// work. Dropping these is safe: pgx already does the right thing for a
// transaction-mode pooler via DefaultQueryExecMode above, and the pool size is
// set here rather than in the URL.
var clientOnlyParams = []string{"pgbouncer", "connection_limit", "pool_timeout", "schema", "prepared_statements"}

// sanitizeDSN removes those parameters, leaving everything else untouched. A
// DSN it cannot parse is passed through unchanged, so pgx reports the real
// problem rather than this function hiding it.
func sanitizeDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.RawQuery == "" {
		return dsn
	}
	q := u.Query()
	dropped := false
	for _, p := range clientOnlyParams {
		if q.Has(p) {
			q.Del(p)
			dropped = true
		}
	}
	if !dropped {
		return dsn
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// wrap turns the Postgres errors the API cares about into the package's own,
// so handlers never import pgx to tell a missing row from a real failure.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrDuplicate, pgErr.ConstraintName)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrNotFound, pgErr.ConstraintName)
		}
	}
	return err
}

const adminGuestCols = `
	g.id, g.name, g.email, g.phone, g.rsvp,
	g.check_in_min, g.check_in_by, g.hadir,
	g.invite, g.invited_at_min, g.invite_error,
	g.reward, g.claimed_at_min, g.invite_token,
	g.outfit_baju, g.outfit_celana, g.outfit_aksesoris,
	(select count(*) from cards c where c.owner_id = g.id) as cards,
	exists (select 1 from final_feedback f where f.guest_id = g.id) as final_feedback`

func scanAdminGuest(row pgx.Row) (model.AdminGuest, error) {
	var g model.AdminGuest
	err := row.Scan(
		&g.ID, &g.Name, &g.Email, &g.Phone, &g.Rsvp,
		&g.CheckIn, &g.CheckInBy, &g.Hadir,
		&g.Invite, &g.InvitedAt, &g.InviteError,
		&g.Reward, &g.ClaimedAt, &g.InviteToken,
		&g.Outfit.Baju, &g.Outfit.Celana, &g.Outfit.Aksesoris,
		&g.Cards, &g.FinalFeedback,
	)
	return g, wrap(err)
}

// ---- guests ----

func (p *Postgres) PresentGuests(ctx context.Context) ([]model.Guest, error) {
	rows, err := p.pool.Query(ctx, `
		select id, name, role, question, outfit, code,
			coalesce((extract(epoch from sprite_at) * 1000)::bigint, 0)
		from guests
		where hadir
		order by created_at, id`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.Guest{} // never nil: an empty list is [] on the wire, not null
	for rows.Next() {
		var g model.Guest
		if err := rows.Scan(&g.ID, &g.Name, &g.Role, &g.Question, &g.Outfit, &g.Code, &g.SpriteAt); err != nil {
			return nil, wrap(err)
		}
		out = append(out, g)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) SetSprite(ctx context.Context, guestID string, raw []byte, mime string) error {
	tag, err := p.pool.Exec(ctx, `
		update guests set sprite = $2, sprite_mime = $3, sprite_at = now() where id = $1`,
		guestID, raw, mime)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) Sprite(ctx context.Context, guestID string) ([]byte, string, error) {
	var raw []byte
	var mime string
	err := p.pool.QueryRow(ctx, `
		select sprite, sprite_mime from guests where id = $1 and sprite is not null`, guestID).Scan(&raw, &mime)
	return raw, mime, wrap(err)
}

func (p *Postgres) AdminGuests(ctx context.Context) ([]model.AdminGuest, error) {
	rows, err := p.pool.Query(ctx, `select`+adminGuestCols+` from guests g order by g.created_at, g.id`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.AdminGuest{}
	for rows.Next() {
		g, err := scanAdminGuest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) AddGuest(ctx context.Context, name, phone, email, inviteToken string) (model.AdminGuest, error) {
	id := lower(name)
	if email == "" {
		email = id + "@contoh.id"
	}

	// Names repeat in a guest list; ids must not. The loop asks Postgres
	// rather than guessing, so two admins importing at once cannot collide.
	for n := 2; ; n++ {
		var taken bool
		if err := p.pool.QueryRow(ctx, `select exists (select 1 from guests where id = $1)`, id).Scan(&taken); err != nil {
			return model.AdminGuest{}, wrap(err)
		}
		if !taken {
			break
		}
		id = lower(name) + "-" + strconv.Itoa(n)
	}

	row := p.pool.QueryRow(ctx, `
		with inserted as (
			insert into guests (id, name, email, phone, invite_token, question, code)
			values ($1, $2, $3, $4, $5, $6, $7)
			returning *
		)
		select`+adminGuestCols+` from inserted g`,
		id, name, email, phone, inviteToken,
		"Kenalan dulu yuk, kamu paling suka ngerjain apa?",
		// A deterministic 4-digit kenalan code, same scheme as the seed.
		strconv.Itoa(1000+hash4(id)),
	)
	return scanAdminGuest(row)
}

func (p *Postgres) GuestByInviteToken(ctx context.Context, tok string) (model.AdminGuest, error) {
	var revoked bool
	if err := p.pool.QueryRow(ctx, `select revoked from guests where invite_token = $1`, tok).Scan(&revoked); err != nil {
		return model.AdminGuest{}, wrap(err)
	}
	if revoked {
		return model.AdminGuest{}, ErrRevoked
	}
	row := p.pool.QueryRow(ctx, `select`+adminGuestCols+` from guests g where g.invite_token = $1`, tok)
	return scanAdminGuest(row)
}

func (p *Postgres) SetRsvp(ctx context.Context, guestID string, r model.Rsvp) error {
	return p.exec(ctx, `update guests set rsvp = $2 where id = $1`, guestID, string(r))
}

func (p *Postgres) CheckIn(ctx context.Context, guestID string, minute int, by string) error {
	// coalesce keeps the first arrival time: that is what the committee
	// reports on, and a reload must not rewrite it. Arriving also implies
	// "ya", whatever the RSVP said.
	return p.exec(ctx, `
		update guests set
			check_in_min = coalesce(check_in_min, $2),
			check_in_by  = case when check_in_min is null then $3 else check_in_by end,
			rsvp         = 'ya'
		where id = $1`, guestID, minute, by)
}

// UndoCheckIn also clears "hadir": a guest who was never at the door cannot
// have finished their character there either, and leaving the flag set would
// keep them on the kenalan list everybody else reads.
func (p *Postgres) UndoCheckIn(ctx context.Context, guestID string) error {
	return p.exec(ctx, `
		update guests set check_in_min = null, check_in_by = '', hadir = false
		where id = $1`, guestID)
}

// ClaimReward moves "siap" to "diambil", and only that.
//
// The `where reward = 'siap'` is the whole safety mechanism: a prize already
// claimed, or not yet earned, matches nothing and the caller gets
// ErrConflict. Two committee phones tapping the same guest therefore hand out
// one prize, decided by Postgres rather than by whoever's network was faster.
func (p *Postgres) ClaimReward(ctx context.Context, guestID string, minute int) error {
	tag, err := p.pool.Exec(ctx, `
		update guests set reward = 'diambil', claimed_at_min = $2
		where id = $1 and reward = 'siap'`, guestID, minute)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		// Tell the two cases apart, because they mean opposite things to the
		// person holding the prize.
		var reward string
		if err := p.pool.QueryRow(ctx, `select reward from guests where id = $1`, guestID).Scan(&reward); err != nil {
			return wrap(err)
		}
		if reward == "diambil" {
			return fmt.Errorf("%w: hadiah sudah diambil", ErrConflict)
		}
		return fmt.Errorf("%w: kartu kenalan belum lengkap", ErrConflict)
	}
	return nil
}

func (p *Postgres) SetHadir(ctx context.Context, guestID string, hadir bool) error {
	return p.exec(ctx, `update guests set hadir = $2 where id = $1`, guestID, hadir)
}

// SetOutfit writes the three choices and the card label together.
//
// One statement, so the label can never disagree with the choice it is
// supposed to summarise. The coalesce chain is model.Outfit.Label in SQL;
// keeping them in step is the price of storing the summary at all, and it
// buys PresentGuests a plain column read instead of a join.
func (p *Postgres) SetOutfit(ctx context.Context, guestID string, o model.Outfit) error {
	return p.exec(ctx, `
		update guests set
			outfit_baju      = $2,
			outfit_celana    = $3,
			outfit_aksesoris = $4,
			outfit           = coalesce(nullif($2, ''), nullif($3, ''), $4)
		where id = $1`,
		guestID, o.Baju, o.Celana, o.Aksesoris)
}

func (p *Postgres) MarkInvite(ctx context.Context, guestIDs []string, status model.InviteStatus, errText string, minute int) error {
	stamp := status == model.InviteSent || status == model.InviteFailed
	// One statement for the whole batch: the committee sends invitations to
	// the entire list at once.
	return p.exec(ctx, `
		update guests set
			invite = $2,
			invite_error = $3,
			invited_at_min = case when $5 then $4 else invited_at_min end
		where id = any($1)`, guestIDs, string(status), errText, minute, stamp)
}

func (p *Postgres) RevokeInvite(ctx context.Context, guestID string) error {
	return p.exec(ctx, `update guests set revoked = true where id = $1`, guestID)
}

// ---- rundown ----

func (p *Postgres) Sessions(ctx context.Context) ([]model.Session, error) {
	rows, err := p.pool.Query(ctx, `
		select id, title, room_id, start_min, end_min, pic, speaker_id, qna
		from sessions order by start_min, id`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.Session{}
	for rows.Next() {
		var s model.Session
		if err := rows.Scan(&s.ID, &s.Title, &s.RoomID, &s.Start, &s.End, &s.PIC, &s.SpeakerID, &s.QnA); err != nil {
			return nil, wrap(err)
		}
		out = append(out, s)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) SaveSessions(ctx context.Context, sessions []model.Session) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrap(err)
	}
	defer tx.Rollback(ctx)

	// Upsert then prune, inside one transaction: the rundown must never be
	// momentarily empty while the committee is editing it, and questions hang
	// off sessions by foreign key.
	keep := make([]string, 0, len(sessions))
	for _, s := range sessions {
		keep = append(keep, s.ID)
		_, err := tx.Exec(ctx, `
			insert into sessions (id, title, room_id, start_min, end_min, pic, speaker_id, qna)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			on conflict (id) do update set
				title = excluded.title, room_id = excluded.room_id,
				start_min = excluded.start_min, end_min = excluded.end_min,
				pic = excluded.pic, speaker_id = excluded.speaker_id, qna = excluded.qna`,
			s.ID, s.Title, s.RoomID, s.Start, s.End, s.PIC, s.SpeakerID, s.QnA)
		if err != nil {
			return wrap(err)
		}
	}
	if _, err := tx.Exec(ctx, `delete from sessions where not (id = any($1))`, keep); err != nil {
		return wrap(err)
	}
	return wrap(tx.Commit(ctx))
}

// ---- questions ----

func (p *Postgres) Questions(ctx context.Context, eventID string) ([]model.Question, error) {
	rows, err := p.pool.Query(ctx, `
		select id, event_id, body, author, author_guest_id, status, at_min, edited
		from questions
		where ($1 = '' or event_id = $1)
		order by at_min, created_at`, eventID)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.Question{}
	for rows.Next() {
		var q model.Question
		if err := rows.Scan(&q.ID, &q.EventID, &q.Text, &q.Author, &q.AuthorGuestID, &q.Status, &q.At, &q.Edited); err != nil {
			return nil, wrap(err)
		}
		out = append(out, q)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) AddQuestion(ctx context.Context, q model.Question) (model.Question, error) {
	if q.ID == "" {
		q.ID = "q" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	}
	if q.Status == "" {
		q.Status = model.QuestionSent
	}
	err := p.pool.QueryRow(ctx, `
		insert into questions (id, event_id, body, author, author_guest_id, status, at_min)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning id, event_id, body, author, author_guest_id, status, at_min, edited`,
		q.ID, q.EventID, q.Text, q.Author, q.AuthorGuestID, string(q.Status), q.At,
	).Scan(&q.ID, &q.EventID, &q.Text, &q.Author, &q.AuthorGuestID, &q.Status, &q.At, &q.Edited)
	return q, wrap(err)
}

func (p *Postgres) EditQuestion(ctx context.Context, id, guestID, text string) (model.Question, error) {
	var q model.Question
	// The where clause carries the authorisation: a question belonging to
	// somebody else simply does not match, so there is no window between
	// checking and writing.
	err := p.pool.QueryRow(ctx, `
		update questions set body = $3, edited = true
		where id = $1 and author_guest_id = $2 and status = 'terkirim'
		returning id, event_id, body, author, author_guest_id, status, at_min, edited`,
		id, guestID, text,
	).Scan(&q.ID, &q.EventID, &q.Text, &q.Author, &q.AuthorGuestID, &q.Status, &q.At, &q.Edited)

	if errors.Is(wrap(err), ErrNotFound) {
		// Nothing matched: say which of the three reasons it was.
		return model.Question{}, p.explainQuestionMiss(ctx, id, guestID)
	}
	return q, wrap(err)
}

func (p *Postgres) DeleteQuestion(ctx context.Context, id, guestID string) error {
	tag, err := p.pool.Exec(ctx, `delete from questions where id = $1 and author_guest_id = $2`, id, guestID)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return p.explainQuestionMiss(ctx, id, guestID)
	}
	return nil
}

// explainQuestionMiss turns "no rows matched" into the reason, so the guest
// sees "the MC already read it" rather than a blank failure.
func (p *Postgres) explainQuestionMiss(ctx context.Context, id, guestID string) error {
	var owner, status string
	err := p.pool.QueryRow(ctx, `select author_guest_id, status from questions where id = $1`, id).Scan(&owner, &status)
	if err != nil {
		return wrap(err) // genuinely gone
	}
	if owner != guestID {
		return ErrForbidden
	}
	return fmt.Errorf("%w: question already read by the MC", ErrConflict)
}

func (p *Postgres) SetQuestionStatus(ctx context.Context, id string, s model.QuestionStatus) error {
	return p.exec(ctx, `update questions set status = $2 where id = $1`, id, string(s))
}

// ---- kenalan cards ----

func (p *Postgres) AddCard(ctx context.Context, ownerID, fromID, note string) (int, error) {
	if ownerID == fromID {
		return 0, fmt.Errorf("%w: cannot collect your own card", ErrForbidden)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, wrap(err)
	}
	defer tx.Rollback(ctx)

	// do nothing on conflict is what makes a replayed collect harmless: the
	// prize counter is the row count, so it cannot be advanced twice.
	if _, err := tx.Exec(ctx, `
		insert into cards (owner_id, from_id, note) values ($1, $2, $3)
		on conflict (owner_id, from_id) do nothing`, ownerID, fromID, note); err != nil {
		return 0, wrap(err)
	}

	var count int
	if err := tx.QueryRow(ctx, `select count(*) from cards where owner_id = $1`, ownerID).Scan(&count); err != nil {
		return 0, wrap(err)
	}
	// Promote to "siap" once, without ever walking back a claimed prize.
	if _, err := tx.Exec(ctx, `
		update guests set reward = 'siap'
		where id = $1 and reward = 'mengumpulkan' and $2 >= 5`, ownerID, count); err != nil {
		return 0, wrap(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, wrap(err)
	}
	return count, nil
}

func (p *Postgres) Cards(ctx context.Context, ownerID string) ([]model.Card, error) {
	rows, err := p.pool.Query(ctx, `
		select from_id, note from cards where owner_id = $1 order by from_id`, ownerID)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.Card{}
	for rows.Next() {
		var c model.Card
		if err := rows.Scan(&c.GuestID, &c.Note); err != nil {
			return nil, wrap(err)
		}
		out = append(out, c)
	}
	return out, wrap(rows.Err())
}

// ---- feedback ----

func (p *Postgres) AddEventFeedback(ctx context.Context, guestID string, f model.EventFeedback) error {
	return p.exec(ctx, `
		insert into event_feedback (event_id, guest_id, rating, body, at_min)
		values ($1, $2, $3, $4, $5)
		on conflict (event_id, guest_id) do update set
			rating = excluded.rating, body = excluded.body, at_min = excluded.at_min`,
		f.EventID, guestID, f.Rating, f.Text, f.At)
}

func (p *Postgres) AddFinalFeedback(ctx context.Context, guestID string, f model.FinalFeedback) error {
	moods := f.Moods
	if moods == nil {
		moods = []string{}
	}
	return p.exec(ctx, `
		insert into final_feedback (guest_id, rating, note, moods, at_min)
		values ($1, $2, $3, $4, $5)
		on conflict (guest_id) do update set
			rating = excluded.rating, note = excluded.note,
			moods = excluded.moods, at_min = excluded.at_min`,
		guestID, f.Rating, f.Note, moods, f.At)
}

func (p *Postgres) EventFeedback(ctx context.Context) (map[string][]model.EventFeedback, error) {
	rows, err := p.pool.Query(ctx, `
		select event_id, rating, body, at_min from event_feedback order by event_id, at_min`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := map[string][]model.EventFeedback{}
	for rows.Next() {
		var f model.EventFeedback
		if err := rows.Scan(&f.EventID, &f.Rating, &f.Text, &f.At); err != nil {
			return nil, wrap(err)
		}
		out[f.EventID] = append(out[f.EventID], f)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) RatedEvents(ctx context.Context, guestID string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `select event_id from event_feedback where guest_id = $1`, guestID)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrap(err)
		}
		out = append(out, id)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) FinalFeedback(ctx context.Context) ([]model.FinalFeedback, error) {
	rows, err := p.pool.Query(ctx, `
		select rating, note, moods, at_min from final_feedback order by at_min`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.FinalFeedback{}
	for rows.Next() {
		var f model.FinalFeedback
		if err := rows.Scan(&f.Rating, &f.Note, &f.Moods, &f.At); err != nil {
			return nil, wrap(err)
		}
		out = append(out, f)
	}
	return out, wrap(rows.Err())
}

// ---- announcements ----

func (p *Postgres) Announcements(ctx context.Context) ([]model.Announcement, error) {
	rows, err := p.pool.Query(ctx, `
		select id, title, body, room_id, at_min, recipients, read_count, failed, source
		from announcements order by at_min, created_at`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.Announcement{}
	for rows.Next() {
		var a model.Announcement
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.RoomID, &a.At, &a.Recipients, &a.Read, &a.Failed, &a.Source); err != nil {
			return nil, wrap(err)
		}
		out = append(out, a)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) AddAnnouncement(ctx context.Context, a model.Announcement) (model.Announcement, error) {
	if a.ID == "" {
		a.ID = "a" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	}
	// Recipients defaults to everybody actually in the building, counted here
	// rather than passed in, so the number cannot be wrong.
	if a.Source == "" {
		a.Source = "panitia"
	}
	err := p.pool.QueryRow(ctx, `
		insert into announcements (id, title, body, room_id, at_min, recipients, source)
		values ($1, $2, $3, $4, $5,
			case when $6 > 0 then $6
			     else (select count(*) from guests where check_in_min is not null) end,
			$7)
		returning id, title, body, room_id, at_min, recipients, read_count, failed, source`,
		a.ID, a.Title, a.Body, a.RoomID, a.At, a.Recipients, a.Source,
	).Scan(&a.ID, &a.Title, &a.Body, &a.RoomID, &a.At, &a.Recipients, &a.Read, &a.Failed, &a.Source)
	return a, wrap(err)
}

func (p *Postgres) DeleteAnnouncement(ctx context.Context, id string) error {
	tag, err := p.pool.Exec(ctx, `delete from announcements where id = $1`, id)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) AddKenalanInvite(ctx context.Context, fromID, toID string, minute int, cooldown time.Duration) (model.KenalanInvite, error) {
	inv := model.KenalanInvite{ID: newID("k"), FromID: fromID, ToID: toID, At: minute}
	var present bool
	if err := p.pool.QueryRow(ctx, `select hadir from guests where id = $1`, toID).Scan(&present); err != nil {
		return inv, wrap(err)
	}
	if !present {
		return inv, ErrNotFound
	}
	// One statement, so two taps racing cannot both slip under the cooldown.
	var sentAt time.Time
	err := p.pool.QueryRow(ctx, `
		insert into kenalan_invites (id, from_id, to_id, at_min)
		select $1, $2, $3, $4
		where not exists (
			select 1 from kenalan_invites
			where from_id = $2 and to_id = $3 and created_at > now() - make_interval(secs => $5)
		)
		returning created_at`,
		inv.ID, fromID, toID, minute, cooldown.Seconds()).Scan(&sentAt)
	if errors.Is(wrap(err), ErrNotFound) {
		return inv, fmt.Errorf("%w: kamu baru saja mengajaknya, coba lagi nanti", ErrConflict)
	}
	if err != nil {
		return inv, wrap(err)
	}
	inv.SentAt = sentAt.UnixMilli()
	return inv, nil
}

func (p *Postgres) KenalanInvites(ctx context.Context, guestID string) ([]model.KenalanInvite, []model.KenalanInvite, error) {
	scan := func(sql string) ([]model.KenalanInvite, error) {
		rows, err := p.pool.Query(ctx, sql, guestID)
		if err != nil {
			return nil, wrap(err)
		}
		defer rows.Close()
		out := []model.KenalanInvite{}
		for rows.Next() {
			var k model.KenalanInvite
			var at time.Time
			if err := rows.Scan(&k.ID, &k.FromID, &k.FromName, &k.ToID, &k.At, &at); err != nil {
				return nil, wrap(err)
			}
			k.SentAt = at.UnixMilli()
			out = append(out, k)
		}
		return out, wrap(rows.Err())
	}
	received, err := scan(`
		select k.id, k.from_id, g.name, k.to_id, k.at_min, k.created_at
		from kenalan_invites k join guests g on g.id = k.from_id
		where k.to_id = $1
		order by k.created_at desc
		limit 50`)
	if err != nil {
		return nil, nil, err
	}
	sent, err := scan(`
		select distinct on (k.to_id) k.id, k.from_id, g.name, k.to_id, k.at_min, k.created_at
		from kenalan_invites k join guests g on g.id = k.from_id
		where k.from_id = $1
		order by k.to_id, k.created_at desc`)
	return received, sent, err
}

func (p *Postgres) ResendAnnouncement(ctx context.Context, id string, minute int) (model.Announcement, error) {
	var a model.Announcement
	// Recipients is recounted: a resend reaches everybody here now, including
	// whoever arrived after the first send.
	err := p.pool.QueryRow(ctx, `
		update announcements set at_min = $2, failed = false,
			recipients = (select count(*) from guests where check_in_min is not null)
		where id = $1
		returning id, title, body, room_id, at_min, recipients, read_count, failed, source`,
		id, minute,
	).Scan(&a.ID, &a.Title, &a.Body, &a.RoomID, &a.At, &a.Recipients, &a.Read, &a.Failed, &a.Source)
	return a, wrap(err)
}

// ---- people ----

func (p *Postgres) People(ctx context.Context) ([]model.Person, error) {
	rows, err := p.pool.Query(ctx, `
		select id, name, role, title, phone,
			case when revoked then 'dicabut'
			     when last_login_at is not null then 'masuk'
			     when invited_at is not null then 'terkirim'
			     else 'belum' end
		from people order by role, name`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.Person{}
	for rows.Next() {
		var pr model.Person
		if err := rows.Scan(&pr.ID, &pr.Name, &pr.Role, &pr.Title, &pr.Phone, &pr.Access); err != nil {
			return nil, wrap(err)
		}
		out = append(out, pr)
	}
	return out, wrap(rows.Err())
}

func (p *Postgres) SavePerson(ctx context.Context, pr model.Person) (model.Person, error) {
	if pr.ID == "" {
		pr.ID = newID("p")
		pr.Access = "belum"
		_, err := p.pool.Exec(ctx, `
			insert into people (id, name, role, title, phone) values ($1, $2, $3, $4, $5)`,
			pr.ID, pr.Name, pr.Role, pr.Title, pr.Phone)
		return pr, wrap(err)
	}
	err := p.pool.QueryRow(ctx, `
		update people set name = $2, role = $3, title = $4, phone = $5 where id = $1
		returning case when revoked then 'dicabut'
		               when last_login_at is not null then 'masuk'
		               when invited_at is not null then 'terkirim'
		               else 'belum' end`,
		pr.ID, pr.Name, pr.Role, pr.Title, pr.Phone).Scan(&pr.Access)
	return pr, wrap(err)
}

// personExec runs an update against one person and reports a missing one.
func (p *Postgres) personExec(ctx context.Context, sql string, args ...any) error {
	tag, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) MarkPersonInvited(ctx context.Context, id string) error {
	return p.personExec(ctx, `update people set invited_at = now() where id = $1`, id)
}

func (p *Postgres) MarkPersonLogin(ctx context.Context, id string) error {
	return p.personExec(ctx, `update people set last_login_at = now() where id = $1`, id)
}

func (p *Postgres) SetPersonRevoked(ctx context.Context, id string, revoked bool) error {
	// Restoring a link also forgets the old sign-in, so the page reads
	// "terkirim" again until they actually come back in.
	return p.personExec(ctx, `
		update people set revoked = $2,
			last_login_at = case when $2 then last_login_at else null end
		where id = $1`, id, revoked)
}

func (p *Postgres) DeletePerson(ctx context.Context, id string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return wrap(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit

	tag, err := tx.Exec(ctx, `delete from people where id = $1`, id)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `update sessions set speaker_id = '' where speaker_id = $1`, id); err != nil {
		return wrap(err)
	}
	return wrap(tx.Commit(ctx))
}

// ---- stage ----

func (p *Postgres) StageState(ctx context.Context) (int64, json.RawMessage, error) {
	var rev int64
	var state []byte
	err := p.pool.QueryRow(ctx, `select rev, state from stage_state where id = 1`).Scan(&rev, &state)
	if errors.Is(wrap(err), ErrNotFound) {
		// The migration seeds this row; a missing one means an empty stage,
		// not a broken request.
		return 0, nil, nil
	}
	return rev, json.RawMessage(state), wrap(err)
}

func (p *Postgres) SaveStageState(ctx context.Context, rev int64, state json.RawMessage) (int64, error) {
	var next int64
	// The rev in the where clause is the lock. rev < 0 means "overwrite
	// whatever is there", which is what a review case seeding the stage does
	// on purpose.
	//
	// The state goes as a string, not []byte: under the simple protocol
	// []byte is sent as a bytea literal, which jsonb refuses.
	err := p.pool.QueryRow(ctx, `
		update stage_state set rev = rev + 1, state = $2::jsonb, updated_at = now()
		where id = 1 and ($1 < 0 or rev = $1)
		returning rev`, rev, string(state)).Scan(&next)

	if errors.Is(wrap(err), ErrNotFound) {
		current, _, _ := p.StageState(ctx)
		return current, ErrConflict
	}
	return next, wrap(err)
}

// ---- dashboard ----

func (p *Postgres) Dashboard(ctx context.Context) (model.Dashboard, error) {
	d := model.Dashboard{RatingCounts: map[string]int{}}

	// One round trip: counting a few hundred guests in SQL beats shipping
	// every row to the API to be counted in a loop.
	err := p.pool.QueryRow(ctx, `
		select
			(select count(*) from guests),
			(select count(*) from guests where rsvp = 'ya'),
			(select count(*) from guests where rsvp = 'tidak'),
			(select count(*) from guests where rsvp = 'belum'),
			(select count(*) from guests where check_in_min is not null),
			(select count(*) from guests where hadir),
			(select count(*) from guests where reward = 'siap'),
			(select count(*) from guests where reward = 'diambil'),
			(select count(*) from questions),
			(select count(*) from final_feedback)`,
	).Scan(&d.Invited, &d.RsvpYes, &d.RsvpNo, &d.RsvpPending,
		&d.CheckedIn, &d.Hadir, &d.RewardsReady, &d.RewardsTaken,
		&d.Questions, &d.FinalFeedback)
	if err != nil {
		return d, wrap(err)
	}

	rows, err := p.pool.Query(ctx, `select rating, count(*) from final_feedback group by rating`)
	if err != nil {
		return d, wrap(err)
	}
	defer rows.Close()
	for rows.Next() {
		var rating, n int
		if err := rows.Scan(&rating, &n); err != nil {
			return d, wrap(err)
		}
		d.RatingCounts[strconv.Itoa(rating)] = n
	}
	return d, wrap(rows.Err())
}

// exec runs a statement that must touch exactly one row, so a typo'd id is
// reported as ErrNotFound instead of succeeding silently.
func (p *Postgres) exec(ctx context.Context, sql string, args ...any) error {
	tag, err := p.pool.Exec(ctx, sql, args...)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// hash4 is a small deterministic spread for generated kenalan codes: the same
// guest id always yields the same 4 digits, which matters because the codes
// get typed in by hand.
func hash4(s string) int {
	h := 0
	for _, c := range s {
		h = (h*31 + int(c)) % 9000
	}
	return h
}

// SeedIfEmpty loads the sample data into a fresh database, so a brand new
// Supabase project behaves like the in-memory mode and the whole workflow is
// testable immediately. It does nothing once there is a single guest.
func (p *Postgres) SeedIfEmpty(ctx context.Context) (bool, error) {
	var n int
	if err := p.pool.QueryRow(ctx, `select count(*) from guests`).Scan(&n); err != nil {
		return false, wrap(err)
	}
	if n > 0 {
		return false, nil
	}

	rows, err := buildGuests()
	if err != nil {
		return false, err
	}
	// Before inserting the guests: this rewrites each one's card count to
	// match the rows it is about to create, so the count this store derives by
	// counting rows agrees with the in-memory one.
	cards := seedCards(rows)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, wrap(err)
	}
	defer tx.Rollback(ctx)

	// Sessions first: questions reference them.
	for _, s := range seedSessions() {
		if _, err := tx.Exec(ctx, `
			insert into sessions (id, title, room_id, start_min, end_min, pic, speaker_id, qna)
			values ($1, $2, $3, $4, $5, $6, $7, $8) on conflict (id) do nothing`,
			s.ID, s.Title, s.RoomID, s.Start, s.End, s.PIC, s.SpeakerID, s.QnA); err != nil {
			return false, wrap(err)
		}
	}
	for _, pr := range seedPeople() {
		if _, err := tx.Exec(ctx, `
			insert into people (id, name, role, title, phone)
			values ($1, $2, $3, $4, $5) on conflict (id) do nothing`,
			pr.ID, pr.Name, pr.Role, pr.Title, pr.Phone); err != nil {
			return false, wrap(err)
		}
	}
	for _, g := range rows {
		if _, err := tx.Exec(ctx, `
			insert into guests (
				id, name, email, phone, rsvp, check_in_min, check_in_by, hadir,
				invite, invited_at_min, reward, claimed_at_min, invite_token,
				role, question, code, outfit,
				outfit_baju, outfit_celana, outfit_aksesoris)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
			g.ID, g.Name, g.Email, g.Phone, string(g.Rsvp), g.CheckIn, g.CheckInBy, g.Hadir,
			string(g.Invite), g.InvitedAt, string(g.Reward), g.ClaimedAt, g.InviteToken,
			g.profile.Role, g.profile.Question, g.profile.Code, g.Outfit.Label(),
			g.Outfit.Baju, g.Outfit.Celana, g.Outfit.Aksesoris); err != nil {
			return false, wrap(err)
		}
	}
	for _, q := range seedQuestions() {
		if _, err := tx.Exec(ctx, `
			insert into questions (id, event_id, body, author, author_guest_id, status, at_min)
			values ($1, $2, $3, $4, $5, $6, $7) on conflict (id) do nothing`,
			q.ID, q.EventID, q.Text, q.Author, q.AuthorGuestID, string(q.Status), q.At); err != nil {
			return false, wrap(err)
		}
	}
	for _, a := range seedAnnouncements() {
		if _, err := tx.Exec(ctx, `
			insert into announcements (id, title, body, room_id, at_min, recipients, read_count)
			values ($1, $2, $3, $4, $5, $6, $7) on conflict (id) do nothing`,
			a.ID, a.Title, a.Body, a.RoomID, a.At, a.Recipients, a.Read); err != nil {
			return false, wrap(err)
		}
	}
	for owner, held := range cards {
		for from, note := range held {
			if _, err := tx.Exec(ctx, `
				insert into cards (owner_id, from_id, note) values ($1, $2, $3)
				on conflict do nothing`, owner, from, note); err != nil {
				return false, wrap(err)
			}
		}
	}
	return true, wrap(tx.Commit(ctx))
}

// ---- committee settings ----

const settingsCols = `
	event_name, event_date, place_name, place_address,
	door_code, panitia_pin, countdown, final_feedback_min, codes_changed_min,
	invite_template, contact_name, contact_phone`

func (p *Postgres) Settings(ctx context.Context) (model.Settings, error) {
	var s model.Settings
	var day time.Time
	err := p.pool.QueryRow(ctx, `select`+settingsCols+` from settings where id = 1`).Scan(
		&s.EventName, &day, &s.Place.Name, &s.Place.Address,
		&s.DoorCode, &s.PanitiaPIN, &s.Countdown, &s.FinalFeedbackAt, &s.CodesChangedAt,
		&s.InviteTemplate, &s.Contact.Name, &s.Contact.Phone,
	)
	if err != nil {
		return model.Settings{}, wrap(err)
	}
	// A `date` column comes back as midnight UTC. Only the calendar day is
	// meaningful, so it is formatted rather than carried as an instant.
	s.Date = day.Format("2006-01-02")
	return s, nil
}

func (p *Postgres) SaveSettings(ctx context.Context, s model.Settings) error {
	return p.exec(ctx, `
		update settings set
			event_name = $1, event_date = $2, place_name = $3, place_address = $4,
			door_code = $5, panitia_pin = $6, countdown = $7,
			final_feedback_min = $8, codes_changed_min = $9,
			invite_template = $10, contact_name = $11, contact_phone = $12,
			updated_at = now()
		where id = 1`,
		s.EventName, s.Date, s.Place.Name, s.Place.Address,
		s.DoorCode, s.PanitiaPIN, s.Countdown, s.FinalFeedbackAt, s.CodesChangedAt,
		s.InviteTemplate, s.Contact.Name, s.Contact.Phone)
}

// ---- the shared gallery ----

const photoCols = `id, guest_id, author, room_id, caption, at_min, censored, censor_reason, checked`

func scanPhoto(row pgx.Row) (model.Photo, error) {
	var p model.Photo
	err := row.Scan(&p.ID, &p.GuestID, &p.Author, &p.RoomID, &p.Caption, &p.At, &p.Censored, &p.CensorReason, &p.Checked)
	return p, wrap(err)
}

func (p *Postgres) Photos(ctx context.Context) ([]model.Photo, error) {
	rows, err := p.pool.Query(ctx, `select `+photoCols+` from photos order by at_min desc, id desc`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	out := []model.Photo{}
	for rows.Next() {
		ph, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ph)
	}
	return out, wrap(rows.Err())
}

// AddPhoto counts and inserts in one statement.
//
// The `where (select count(*) ...) < $N` is the limit: a guest who taps share
// twice on a slow connection cannot slip past a count read a moment earlier.
// Zero rows inserted means they are at the cap.
func (p *Postgres) AddPhoto(ctx context.Context, up model.PhotoUpload, limit int) (model.Photo, error) {
	id := newID("ph")
	row := p.pool.QueryRow(ctx, `
		insert into photos (id, guest_id, author, room_id, caption, at_min, mime, bytes)
		select $1, $2, $3, $4, $5, $6, $7, $8
		where (select count(*) from photos where guest_id = $2) < $9
		returning `+photoCols,
		id, up.GuestID, up.Author, up.RoomID, up.Caption, up.At, up.Mime, up.Bytes, limit)

	ph, err := scanPhoto(row)
	if errors.Is(err, ErrNotFound) {
		return model.Photo{}, fmt.Errorf("%w: sudah mencapai batas foto", ErrConflict)
	}
	return ph, err
}

func (p *Postgres) DeletePhoto(ctx context.Context, id, guestID string) error {
	var owner string
	if err := p.pool.QueryRow(ctx, `select guest_id from photos where id = $1`, id).Scan(&owner); err != nil {
		return wrap(err)
	}
	if owner != guestID {
		return ErrForbidden
	}
	return p.exec(ctx, `delete from photos where id = $1`, id)
}

func (p *Postgres) PhotoBytes(ctx context.Context, id string) ([]byte, string, error) {
	var b []byte
	var mime string
	if err := p.pool.QueryRow(ctx, `select bytes, mime from photos where id = $1`, id).Scan(&b, &mime); err != nil {
		return nil, "", wrap(err)
	}
	return b, mime, nil
}

func (p *Postgres) CensorPhotos(ctx context.Context, ids []string, censored bool, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	// Lifting a censor clears the note with it: a reason left behind would
	// describe a decision that has been reversed.
	if !censored {
		reason = ""
	}
	return p.exec(ctx, `update photos set censored = $2, censor_reason = $3 where id = any($1)`, ids, censored, reason)
}

func (p *Postgres) MarkPhotosChecked(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return p.exec(ctx, `update photos set checked = true where id = any($1)`, ids)
}

// newID makes a collision-free id.
//
// Questions use a millisecond timestamp, which is fine when a guest sends one
// at a time. Photos are shared five at once, in the same millisecond, so they
// need actual randomness. A failure falls back to the timestamp rather than
// refusing the upload: a vanishingly rare duplicate is caught by the primary
// key, and losing a guest's photo to a dry entropy pool would be worse.
func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return prefix + hex.EncodeToString(b)
}
