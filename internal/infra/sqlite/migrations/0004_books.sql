-- +goose Up
-- Phase 2.5 (ADR 0006): books become a live media kind. The author column
-- serves book items (empty for movies/series); the two book quality
-- profiles reuse the shared quality model with format-as-source.

ALTER TABLE media_items ADD COLUMN author TEXT NOT NULL DEFAULT '';

INSERT INTO quality_profiles (id, name, definition, upgrades_allowed) VALUES
 (4, 'Ebook', '{"allowed":[{"source":"pdf","resolution":0},{"source":"mobi","resolution":0},{"source":"azw3","resolution":0},{"source":"epub","resolution":0}],"cutoff":{"source":"epub","resolution":0}}', 1),
 (5, 'Audiobook', '{"allowed":[{"source":"mp3","resolution":0},{"source":"m4b","resolution":0}],"cutoff":{"source":"m4b","resolution":0}}', 1);

-- Books are identified by Open Library work id; one library entry per work.
CREATE UNIQUE INDEX idx_media_items_kind_olid
    ON media_items (kind, olid) WHERE olid != '';
