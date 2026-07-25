-- +goose Up
-- ADR 0012: a library record no provider backs.
--
-- `source` names which provider a row came from, and 'manual' means none did.
-- An explicit discriminator rather than "has no external ids", because a zero
-- id already means "not known" everywhere else in this schema and overloading
-- it to also mean "there is nothing to know" is how the two become
-- indistinguishable — the exact trap ADR 0011 §4 called out for tvdb_id.
--
-- Values: tmdb · tvdb · tvmaze · openlibrary · manual · '' (pre-existing rows
-- whose provider was inferred below).
ALTER TABLE media_items ADD COLUMN source TEXT NOT NULL DEFAULT '';

-- Backfill by the ids a row actually carries. Order matters: a TMDB series
-- usually also has a tvdb_id (TMDB hands it over in external_ids), so TMDB is
-- checked first and only rows with a tvdb_id and no tmdb_id are attributed to
-- the chain.
UPDATE media_items SET source = 'openlibrary' WHERE olid != '';
UPDATE media_items SET source = 'tmdb'        WHERE source = '' AND tmdb_id != 0;
UPDATE media_items SET source = 'tvmaze'      WHERE source = '' AND tvdb_id != 0;

-- Anything left has no ids at all. That state was unreachable before this
-- migration (every add went through a provider), so there is nothing to
-- attribute and '' is the honest value.

-- +goose Down
ALTER TABLE media_items DROP COLUMN source;
