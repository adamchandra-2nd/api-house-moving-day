-- Each MC and speaker signs in with a code of their own, sent with their
-- personal link. It replaces the one STAGE_CODE everybody shared, which
-- lived in the server's environment and could only be changed by a deploy.
alter table people add column if not exists stage_code text;

-- Everyone already on the list gets a random six-digit code.
update people
   set stage_code = lpad((floor(random() * 1000000))::int::text, 6, '0')
 where stage_code is null or stage_code = '';
