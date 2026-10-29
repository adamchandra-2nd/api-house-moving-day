-- 002 closed every table that existed then. settings (004) and photos (005)
-- came later and were left open, so Supabase's PostgREST published both:
-- settings carries the door code and the committee PIN, photos every guest's
-- upload. Same treatment as 002: row level security on, no policies, so the
-- anon and authenticated roles see nothing. The Go service connects as the
-- table owner and is unaffected.

alter table settings enable row level security;
alter table photos   enable row level security;

do $$
begin
  if exists (select 1 from pg_roles where rolname = 'anon') then
    revoke all on settings, photos from anon;
  end if;
  if exists (select 1 from pg_roles where rolname = 'authenticated') then
    revoke all on settings, photos from authenticated;
  end if;
end
$$;
