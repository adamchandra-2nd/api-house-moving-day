-- HMD Event Companion -- store the character a guest built.
--
-- Run this after 001_init.sql. Safe to run twice.
--
-- Until now the outfit lived only in the guest's browser (localStorage, see
-- src/guest/state.ts), which meant a guest who opened their invite link on a
-- second phone, or cleared their browser data, lost the character they made
-- and had to build another one. The personal link is the identity in this app,
-- so what the guest made with it belongs next to the guest, not next to the
-- browser.
--
-- Three columns rather than one JSON blob: these are three independent
-- choices from three fixed lists, and the committee will want to count them
-- ("how many hoodies do we need to draw?"), which is a group-by on a column
-- and a parse on a blob.
--
-- `outfit` (already in 001) stays, and keeps its meaning: the one-line label
-- on a kenalan card. It is derived from these -- the API writes both in the
-- same statement -- so there is still only one place a guest's choice is made.

alter table guests add column if not exists outfit_baju      text not null default '';
alter table guests add column if not exists outfit_celana    text not null default '';
alter table guests add column if not exists outfit_aksesoris text not null default '';

-- Backfill, so guests seeded before this migration are not left looking like
-- they never built a character. 001 put the card label in `outfit`, and for
-- the seeded rows that label is always a top.
update guests
   set outfit_baju = outfit
 where outfit <> ''
   and outfit_baju = '';
