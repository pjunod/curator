-- +goose Up
-- Durable release identity: provider-scoped aliases and country snapshots,
-- a cross-process revision, indexed external IDs, and immutable grab evidence.

CREATE TABLE media_aliases (
    id INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    title TEXT NOT NULL CHECK (length(trim(title)) > 0),
    normalized_title TEXT NOT NULL,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL DEFAULT '',
    language TEXT NOT NULL DEFAULT '',
    market_country TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL DEFAULT 'work'
        CHECK (scope IN ('work', 'season', 'unsupported_numbering')),
    role TEXT NOT NULL DEFAULT 'alternate'
        CHECK (role IN ('original', 'alternate', 'historical', 'manual')),
    searchable INTEGER NOT NULL DEFAULT 0 CHECK (searchable IN (0, 1)),
    UNIQUE (media_item_id, normalized_title, source, source_id, scope,
            role, language, market_country)
) STRICT;

CREATE INDEX idx_media_aliases_normalized ON media_aliases(normalized_title);

CREATE TABLE media_identity_revision (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL DEFAULT 0
) STRICT;

INSERT INTO media_identity_revision(id, revision) VALUES (1, 0);

CREATE TABLE media_identity_sources (
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    countries TEXT NOT NULL DEFAULT '[]',
    fetched_at INTEGER NOT NULL DEFAULT 0,
    attempted_at INTEGER NOT NULL DEFAULT 0,
    retry_after INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (media_item_id, source)
) STRICT;

CREATE INDEX idx_media_items_kind_imdb
    ON media_items(kind, imdb_id) WHERE imdb_id != '';
CREATE INDEX idx_media_items_kind_tvdb
    ON media_items(kind, tvdb_id) WHERE tvdb_id != 0;

ALTER TABLE downloads ADD COLUMN match_evidence TEXT NOT NULL DEFAULT '{}';

-- Migration 0017 established the routing rule. Reapply it for any imported
-- or legacy rows that were created with a blank source after that migration.
UPDATE media_items SET source = 'openlibrary'
WHERE source = '' AND olid != '';
UPDATE media_items SET source = 'tmdb'
WHERE source = '' AND tmdb_id != 0;
UPDATE media_items SET source = 'tvmaze'
WHERE source = '' AND tvdb_id != 0;

-- +goose Down
-- The previous binary ignores additive tables/indexes, but cannot read a
-- rebuilt downloads table safely while jobs may be pending. Coordinated
-- rollback restores a backup instead of applying a destructive Down.
SELECT 1;
