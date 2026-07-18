-- +goose Up
-- Multi-source ratings: a JSON array of {source, value, votes, scale}
-- entries — tmdb (/10), openlibrary (/5), and via OMDb: imdb (/10),
-- rt (/100), metacritic (/100). The rating/rating_votes columns from 0008
-- remain the primary (metadata provider) rating used on library cards;
-- this column carries the full labeled set for the detail page.

ALTER TABLE media_items ADD COLUMN ratings TEXT NOT NULL DEFAULT '[]';
