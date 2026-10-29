-- Each guest's character as a picture, so other guests' phones can show it on
-- the kenalan cards. The phone that built it renders it from the 3D scene and
-- uploads it; a webp of a few tens of KB. sprite_at versions the image URL, so
-- phones cache it hard and still see a new one at once.
alter table guests add column if not exists sprite      bytea;
alter table guests add column if not exists sprite_mime text not null default '';
alter table guests add column if not exists sprite_at   timestamptz;
