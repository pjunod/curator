-- +goose Up
-- 0019: a quality profile is a target, not a list with a cutoff (ADR 0014).
--
-- The old definition JSON was {"allowed":[...],"cutoff":{...}} -- two knobs
-- encoding one intent, which is how the seeded default came to be called "Any"
-- and to read, on every item page, "Any -- upgrades until WEB-DL 1080p, then
-- stops". Worse than confusing: the cutoff never bounded what got GRABBED, so
-- a missing item under "Any" could pull an 80 GB 2160p remux for a profile
-- that would have called itself finished at WEB-DL 1080p.
--
-- The new shape is {"target":{...},"floor":{...}|null}. Floor null means "no
-- floor": grab the best thing at or below the target resolution, whatever it
-- is. `upgrades_allowed` is unchanged and still lives in its own column.
--
-- Rows are REWRITTEN IN PLACE, never re-created: media_items, media_copies,
-- and import lists all reference profiles by id, and goose is up-only, so a
-- delete-and-insert here would orphan every library item in the database.
--
-- One behaviour change, and it is the fix rather than a regression: items on
-- the old "Any" with nothing on disk could previously grab 2160p. After this
-- they cap at 1080p -- which is what that profile's cutoff always claimed.

UPDATE quality_profiles
SET name = '1080p',
    definition = '{"target":{"source":"webdl","resolution":1080},"floor":null}'
WHERE id = 1;

-- HD-1080p keeps its meaning exactly: same target, and a floor that refuses
-- anything below 1080p rather than encoding that as a five-entry list.
UPDATE quality_profiles
SET name = 'HD-1080p',
    definition = '{"target":{"source":"webdl","resolution":1080},"floor":{"source":"hdtv","resolution":1080}}'
WHERE id = 2;

-- "Ultra-HD" was a 2160p-only allowed list, which is a floor and a target at
-- the same resolution. The name can now be the thing people call it.
UPDATE quality_profiles
SET name = '4K',
    definition = '{"target":{"source":"webdl","resolution":2160},"floor":{"source":"webdl","resolution":2160}}'
WHERE id = 3;

-- Books ride the same model on the source axis: resolution is 0 on both sides,
-- so the resolution cap is vacuous and "met" reduces to source rank >= target
-- -- precisely the old cutoff semantics for formats (ADR 0014 consequences).
UPDATE quality_profiles
SET definition = '{"target":{"source":"epub","resolution":0},"floor":null}'
WHERE id = 4;

UPDATE quality_profiles
SET definition = '{"target":{"source":"m4b","resolution":0},"floor":null}'
WHERE id = 5;

-- Any profile a user created through the API before this migration (there was
-- no editor, so in practice none) gets its cutoff promoted to its target: the
-- cutoff is the closest thing the old shape had to a statement of intent.
UPDATE quality_profiles
SET definition = '{"target":' || json_extract(definition, '$.cutoff') || ',"floor":null}'
WHERE json_extract(definition, '$.target') IS NULL
  AND json_extract(definition, '$.cutoff') IS NOT NULL;
