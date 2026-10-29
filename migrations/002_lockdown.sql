-- HMD Event Companion -- close the auto-generated REST API.
--
-- Run this in the Supabase SQL editor right after 001_init.sql.
--
-- Supabase publishes every table in the `public` schema through PostgREST,
-- reachable with the project's anon key. This app does not use that API at
-- all: the Go service connects straight to Postgres. So the exposure is pure
-- downside, and on two tables it is a genuine hole --
--
--   guests.invite_token  is every guest's credential. Anyone who could read
--                        this column could open any guest's link as them.
--   guests.phone         is a list of real phone numbers.
--
-- Turning row level security on with *no policies* denies everything to the
-- anon and authenticated roles, which is exactly what we want: nothing should
-- reach these tables except our own service.
--
-- It does not affect the API. RLS is not applied to a table's owner, and the
-- connection string Supabase gives you connects as `postgres`, which owns
-- these tables. Nothing in the Go code changes, and nothing needs a policy.
--
-- Verify afterwards: the globe icon next to each table in the Table Editor
-- goes away, and `select * from guests` still works from the SQL editor.

alter table guests         enable row level security;
alter table sessions       enable row level security;
alter table questions      enable row level security;
alter table cards          enable row level security;
alter table event_feedback enable row level security;
alter table final_feedback enable row level security;
alter table announcements  enable row level security;
alter table people         enable row level security;
alter table stage_state    enable row level security;

-- Belt and braces: stop the two PostgREST roles from reaching the schema in
-- the first place, so a future table created without RLS is not automatically
-- published either.
--
-- Guarded, because `anon` and `authenticated` are Supabase's roles and do not
-- exist in a plain Postgres -- including the one in docker-compose.yml, which
-- runs everything in this directory on first start. Without the guard this
-- file would abort local initialisation.
--
-- Drop this block if you ever decide to use supabase-js from the browser; as
-- long as the Go API is the only client, it costs nothing.
do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    revoke usage on schema public from anon;
    revoke all on all tables in schema public from anon;
  end if;
  if exists (select 1 from pg_roles where rolname = 'authenticated') then
    revoke usage on schema public from authenticated;
    revoke all on all tables in schema public from authenticated;
  end if;
end
$$;
