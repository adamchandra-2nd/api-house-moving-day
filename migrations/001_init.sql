-- HMD Event Companion -- initial schema.
--
-- Run this once in the Supabase SQL editor (Dashboard -> SQL Editor -> New
-- query -> paste -> Run). It is written to be safe to re-run.
--
-- Clock columns ending in _min are rundown minutes (minutes since midnight),
-- the unit the whole app already thinks in. Real instants are timestamptz.

-- ---------------------------------------------------------------- guests

create table if not exists guests (
  id            text primary key,
  name          text        not null,
  email         text        not null default '',
  phone         text        not null,
  rsvp          text        not null default 'belum'
                  check (rsvp in ('ya', 'tidak', 'belum')),

  check_in_min  integer,
  check_in_by   text        not null default '',
  hadir         boolean     not null default false,

  invite        text        not null default 'belum'
                  check (invite in ('belum', 'mengirim', 'terkirim', 'dibaca', 'gagal')),
  invited_at_min integer,
  invite_error  text        not null default '',

  reward        text        not null default 'mengumpulkan'
                  check (reward in ('mengumpulkan', 'siap', 'diambil')),
  claimed_at_min integer,

  -- The personal invite link. It is the guest's credential, so it is unique,
  -- indexed for the one lookup that matters, and revocable without deleting
  -- the guest (their check-in and cards must survive a reissued link).
  invite_token  text        not null unique,
  revoked       boolean     not null default false,

  -- The public kenalan card.
  role          text        not null default 'Tamu',
  question      text        not null default '',
  code          text        not null default '',
  outfit        text        not null default '',

  created_at    timestamptz not null default now()
);

-- One phone number per guest list: the committee imports CSVs more than once
-- and duplicate rows would mean duplicate invitations.
create unique index if not exists guests_phone_key on guests (phone);

-- The hot path: every guest's first request resolves their token.
create index if not exists guests_invite_token_live
  on guests (invite_token) where not revoked;

-- ---------------------------------------------------------------- rundown

create table if not exists sessions (
  id         text    primary key,
  title      text    not null,
  room_id    text    not null,
  start_min  integer not null,
  end_min    integer not null,
  pic        text    not null default '',
  speaker_id text    not null default '',
  qna        boolean not null default false,

  constraint sessions_span check (end_min > start_min)
);

create index if not exists sessions_start on sessions (start_min);

-- ---------------------------------------------------------------- questions

create table if not exists questions (
  id              text    primary key,
  event_id        text    not null references sessions (id) on delete cascade,
  body            text    not null,
  -- null = sent anonymously. The guest id below is still recorded, because
  -- the author must be able to edit or delete their own question.
  author          text,
  author_guest_id text    not null references guests (id) on delete cascade,
  status          text    not null default 'terkirim'
                    check (status in ('terkirim', 'dibaca', 'terjawab')),
  at_min          integer not null,
  edited          boolean not null default false,
  created_at      timestamptz not null default now()
);

create index if not exists questions_event on questions (event_id, at_min);
create index if not exists questions_author on questions (author_guest_id);

-- ---------------------------------------------------------------- cards

-- owner_id collected from_id's card. The composite primary key is what makes
-- collecting the same card twice a no-op instead of a double prize count --
-- important because this is the write most likely to be replayed from a
-- phone's offline queue.
create table if not exists cards (
  owner_id   text not null references guests (id) on delete cascade,
  from_id    text not null references guests (id) on delete cascade,
  note       text not null default '',
  created_at timestamptz not null default now(),

  primary key (owner_id, from_id),
  constraint cards_not_self check (owner_id <> from_id)
);

-- ---------------------------------------------------------------- feedback

-- One rating per guest per session, enforced by the key: a replayed write
-- updates the existing row rather than stacking a second vote.
create table if not exists event_feedback (
  event_id text    not null references sessions (id) on delete cascade,
  guest_id text    not null references guests (id) on delete cascade,
  rating   integer not null check (rating between 1 and 5),
  body     text    not null default '',
  at_min   integer not null,

  primary key (event_id, guest_id)
);

create table if not exists final_feedback (
  guest_id text    primary key references guests (id) on delete cascade,
  rating   integer not null check (rating between 1 and 5),
  note     text    not null default '',
  moods    text[]  not null default '{}',
  at_min   integer not null
);

-- ---------------------------------------------------------------- announcements

create table if not exists announcements (
  id         text    primary key,
  title      text    not null,
  body       text    not null,
  -- null = sent to everybody, not to one room.
  room_id    text,
  at_min     integer not null,
  recipients integer not null default 0,
  read_count integer not null default 0,
  failed     boolean not null default false,
  created_at timestamptz not null default now()
);

-- ---------------------------------------------------------------- people on stage

create table if not exists people (
  id    text primary key,
  name  text not null,
  role  text not null check (role in ('mc', 'pembicara')),
  title text not null default '',
  phone text not null default ''
);

-- ---------------------------------------------------------------- stage state

-- The live MC/speaker state: one row, held as an opaque document.
--
-- Its shape belongs to the frontend (src/stage/store.ts) and changes as the
-- stage sides grow, so re-declaring every field here would only create
-- something to drift from. rev is an optimistic lock: two phones editing the
-- same live state is the normal case, so a writer passes the rev it read and
-- is told to re-read if it lost the race.
create table if not exists stage_state (
  id         integer primary key default 1 check (id = 1),
  rev        bigint  not null default 0,
  state      jsonb,
  updated_at timestamptz not null default now()
);

insert into stage_state (id, rev, state) values (1, 0, null)
  on conflict (id) do nothing;
