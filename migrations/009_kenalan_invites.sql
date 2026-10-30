-- "Ajak kenalan": one guest lets another know they would like to meet. The
-- receiver gets it in Notifikasi; the sender can ask again after a cooldown,
-- which the server enforces from created_at.
create table if not exists kenalan_invites (
  id         text primary key,
  from_id    text        not null references guests(id) on delete cascade,
  to_id      text        not null references guests(id) on delete cascade,
  at_min     integer     not null,
  created_at timestamptz not null default now()
);
create index if not exists kenalan_invites_to   on kenalan_invites (to_id, created_at desc);
create index if not exists kenalan_invites_pair on kenalan_invites (from_id, to_id, created_at desc);

-- Same lockdown as 002 and 006: nothing through PostgREST.
alter table kenalan_invites enable row level security;
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    revoke all on kenalan_invites from anon;
  end if;
  if exists (select 1 from pg_roles where rolname = 'authenticated') then
    revoke all on kenalan_invites from authenticated;
  end if;
end
$$;
