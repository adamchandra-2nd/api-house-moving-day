-- What the committee set up in the browser and lost on every reload, moved to
-- the database so a real event day survives a refreshed laptop.

-- Pengisi acara: where each MC/speaker's personal link stands. invited_at is
-- when the committee sent or copied it, last_login_at when they last typed
-- the stage code, and revoked turns the link -- and every session already
-- signed in with it -- off.
alter table people add column if not exists invited_at    timestamptz;
alter table people add column if not exists last_login_at timestamptz;
alter table people add column if not exists revoked       boolean not null default false;

-- Pengaturan: the WhatsApp invitation text, and who to call. Empty template
-- means the server's built-in one; empty contact means the screens hide the
-- "Hubungi panitia" button rather than dialling nobody.
alter table settings add column if not exists invite_template text not null default '';
alter table settings add column if not exists contact_name    text not null default '';
alter table settings add column if not exists contact_phone   text not null default '';

-- Pengumuman: who sent it. The MC announces from the stage too, and guests
-- see both in the same place.
alter table announcements add column if not exists source text not null default 'panitia';
