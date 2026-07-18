-- +goose Up
-- Community ratings on media items. Scale follows the provider: TMDB rates
-- movies/series 0-10, Open Library rates books 0-5; rating_votes 0 means
-- "no rating known" and the UI hides the star. Populated on add and by the
-- metadata.refresh task (which also backfills items added before this).

ALTER TABLE media_items ADD COLUMN rating       REAL    NOT NULL DEFAULT 0;
ALTER TABLE media_items ADD COLUMN rating_votes INTEGER NOT NULL DEFAULT 0;
