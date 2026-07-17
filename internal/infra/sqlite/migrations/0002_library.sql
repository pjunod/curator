-- +goose Up
-- Phase 1 schema: the library. Design per ADR 0002 (one media_items table,
-- kind column, n:m file<->episode links) and ADR 0006 (kind 'book' and the
-- book external ids reserved from day one). Timestamps are unix millis.

CREATE TABLE root_folders (
    id       INTEGER PRIMARY KEY,
    path     TEXT NOT NULL UNIQUE,
    added_at INTEGER NOT NULL
) STRICT;

CREATE TABLE media_items (
    id             INTEGER PRIMARY KEY,
    kind           TEXT NOT NULL CHECK (kind IN ('movie', 'series', 'book')),
    title          TEXT NOT NULL,
    sort_title     TEXT NOT NULL,
    year           INTEGER NOT NULL DEFAULT 0,

    -- external ids (0/'' = unknown); isbn13/olid/asin serve kind='book'
    tmdb_id        INTEGER NOT NULL DEFAULT 0,
    imdb_id        TEXT NOT NULL DEFAULT '',
    tvdb_id        INTEGER NOT NULL DEFAULT 0,
    isbn13         TEXT NOT NULL DEFAULT '',
    olid           TEXT NOT NULL DEFAULT '',
    asin           TEXT NOT NULL DEFAULT '',

    -- metadata cache
    overview       TEXT NOT NULL DEFAULT '',
    poster_path    TEXT NOT NULL DEFAULT '',
    backdrop_path  TEXT NOT NULL DEFAULT '',
    genres         TEXT NOT NULL DEFAULT '[]',  -- JSON array of strings
    status         TEXT NOT NULL DEFAULT '',
    release_date   TEXT NOT NULL DEFAULT '',    -- ISO date; first air date for series
    runtime        INTEGER NOT NULL DEFAULT 0,  -- minutes

    monitored      INTEGER NOT NULL DEFAULT 1,
    root_folder_id INTEGER REFERENCES root_folders(id) ON DELETE SET NULL,
    path           TEXT NOT NULL DEFAULT '',

    ended          INTEGER NOT NULL DEFAULT 0,  -- series-only

    added_at       INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_media_items_kind_tmdb
    ON media_items (kind, tmdb_id) WHERE tmdb_id != 0;
CREATE INDEX idx_media_items_kind ON media_items (kind);

CREATE TABLE seasons (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    number        INTEGER NOT NULL,
    monitored     INTEGER NOT NULL DEFAULT 1,
    UNIQUE (media_item_id, number)
) STRICT;

CREATE TABLE episodes (
    id             INTEGER PRIMARY KEY,
    media_item_id  INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    season_number  INTEGER NOT NULL,
    episode_number INTEGER NOT NULL,
    absolute_num   INTEGER NOT NULL DEFAULT 0,
    title          TEXT NOT NULL DEFAULT '',
    air_date       TEXT NOT NULL DEFAULT '',
    monitored      INTEGER NOT NULL DEFAULT 1,
    UNIQUE (media_item_id, season_number, episode_number)
) STRICT;

CREATE INDEX idx_episodes_item ON episodes (media_item_id);

CREATE TABLE media_files (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER REFERENCES media_items(id) ON DELETE CASCADE,  -- NULL = unmatched
    path          TEXT NOT NULL UNIQUE,
    size          INTEGER NOT NULL DEFAULT 0,
    added_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_media_files_item ON media_files (media_item_id);

CREATE TABLE media_file_episodes (
    media_file_id INTEGER NOT NULL REFERENCES media_files(id) ON DELETE CASCADE,
    episode_id    INTEGER NOT NULL REFERENCES episodes(id) ON DELETE CASCADE,
    PRIMARY KEY (media_file_id, episode_id)
) STRICT;
