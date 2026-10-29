-- HMD Event Companion -- the shared gallery.
--
-- Run after 004_settings.sql. Safe to run twice.
--
-- Guests share photos from their phone's library; the committee looks through
-- them and can censor one. In the prototype the whole gallery lived in
-- localStorage, which meant every phone had its own: a photo one guest shared
-- never reached anyone else, and a censor the committee applied blurred the
-- tile on their own laptop and nowhere else.
--
-- The bytes live here rather than in object storage. One dependency instead of
-- two, the same code path on a laptop and on Supabase, and photos that are
-- deleted with the row rather than left behind. The cost is database size, so
-- the app resizes before uploading and the API refuses anything larger -- see
-- maxPhotoBytes in internal/httpapi.

create table if not exists photos (
  id          text primary key,

  -- Who shared it. The row goes when the guest does: a guest removed from the
  -- list should not leave photos behind with nobody's name on them.
  guest_id    text        not null references guests(id) on delete cascade,
  -- Denormalised on purpose: the gallery shows the name beside every tile,
  -- and this keeps that a column read rather than a join per photo.
  author      text        not null,

  room_id     text        not null default '',
  caption     text        not null default '',
  -- Rundown minute (minutes since midnight, Asia/Jakarta).
  at_min      integer     not null,

  -- The image itself, already resized by the client.
  mime        text        not null default 'image/jpeg',
  bytes       bytea       not null,

  -- Censored by the committee: other guests get a thumbnail too small to make
  -- anything out, never the full image. The reason is internal and is never
  -- sent to a guest-facing endpoint.
  censored      boolean   not null default false,
  censor_reason text      not null default '',

  -- Whether the committee has looked at it yet. Drives the "foto baru" count.
  checked     boolean     not null default false,

  created_at  timestamptz not null default now()
);

-- The gallery is read newest-first, constantly, by every phone in the room.
create index if not exists photos_feed on photos (at_min desc, id desc);

-- "How many have I shared?" is checked on every upload, against the per-guest
-- limit.
create index if not exists photos_by_guest on photos (guest_id);
