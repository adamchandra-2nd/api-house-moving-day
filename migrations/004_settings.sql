-- HMD Event Companion -- the committee's own settings.
--
-- Run after 003_outfit.sql. Safe to run twice.
--
-- The admin has a Pengaturan page that edits the event name, the date, the
-- venue, the door code, the committee PIN and when the closing feedback
-- opens. Until now it edited React state: the committee changed the door
-- code, the screen said "tersimpan", and the next guest to arrive was still
-- checked in against the code baked into the server's environment. The screen
-- and the system disagreed, silently, about the one thing a door code is for.
--
-- One row, by construction. `id` is fixed at 1 with a check constraint, so a
-- second row cannot be inserted by accident and every read is a plain select
-- with no "which one is current" question to answer.
--
-- Columns rather than a JSON document: these are nine named settings, not an
-- open bag, and two of them -- door_code and panitia_pin -- are credentials
-- the check-in endpoint compares against on every arrival.

create table if not exists settings (
  id                 integer primary key default 1 check (id = 1),

  event_name         text        not null default 'House Moving Day',
  event_date         date        not null default date '2026-11-08',
  place_name         text        not null default 'Kantor baru Potara',
  place_address      text        not null default '',

  -- Typed in by guests at the door and by the committee on a guest's phone.
  door_code          text        not null default '1108',
  panitia_pin        text        not null default '2026',

  -- Whether the pre-event countdown screen is shown at all.
  countdown          boolean     not null default true,

  -- Rundown minutes (minutes since midnight, Asia/Jakarta).
  final_feedback_min integer     not null default 810,
  codes_changed_min  integer     not null default 465,

  updated_at         timestamptz not null default now()
);

-- The defaults above are the whole row, so an empty table and a fresh install
-- start from the same place. Existing installs keep whatever they have.
insert into settings (id) values (1) on conflict (id) do nothing;
