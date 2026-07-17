-- +goose Up
-- Phase 2 schema: quality profiles, indexers, download clients, the
-- download queue, and append-only history. Timestamps are unix millis.

CREATE TABLE quality_profiles (
    id               INTEGER PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    -- JSON: {"allowed":[{"source":"webdl","resolution":1080},...],
    --        "cutoff":{"source":"webdl","resolution":1080}}
    definition       TEXT NOT NULL,
    upgrades_allowed INTEGER NOT NULL DEFAULT 1
) STRICT;

INSERT INTO quality_profiles (id, name, definition, upgrades_allowed) VALUES
 (1, 'Any', '{"allowed":[{"source":"hdtv","resolution":480},{"source":"dvd","resolution":480},{"source":"hdtv","resolution":720},{"source":"webrip","resolution":720},{"source":"webdl","resolution":720},{"source":"bluray","resolution":720},{"source":"hdtv","resolution":1080},{"source":"webrip","resolution":1080},{"source":"webdl","resolution":1080},{"source":"bluray","resolution":1080},{"source":"remux","resolution":1080},{"source":"webdl","resolution":2160},{"source":"bluray","resolution":2160},{"source":"remux","resolution":2160}],"cutoff":{"source":"webdl","resolution":1080}}', 1),
 (2, 'HD-1080p', '{"allowed":[{"source":"hdtv","resolution":1080},{"source":"webrip","resolution":1080},{"source":"webdl","resolution":1080},{"source":"bluray","resolution":1080},{"source":"remux","resolution":1080}],"cutoff":{"source":"webdl","resolution":1080}}', 1),
 (3, 'Ultra-HD', '{"allowed":[{"source":"webdl","resolution":2160},{"source":"bluray","resolution":2160},{"source":"remux","resolution":2160}],"cutoff":{"source":"webdl","resolution":2160}}', 1);

ALTER TABLE media_items ADD COLUMN quality_profile_id INTEGER NOT NULL DEFAULT 1;
ALTER TABLE media_files ADD COLUMN quality TEXT NOT NULL DEFAULT '';

CREATE TABLE indexers (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    url        TEXT NOT NULL,
    api_key    TEXT NOT NULL DEFAULT '',
    -- torznab and newznab are the same API; protocol tells us which
    -- download client family the results feed.
    protocol   TEXT NOT NULL CHECK (protocol IN ('torrent', 'usenet')),
    categories TEXT NOT NULL DEFAULT '[]',
    enabled    INTEGER NOT NULL DEFAULT 1,
    added_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE download_clients (
    id       INTEGER PRIMARY KEY,
    type     TEXT NOT NULL CHECK (type IN ('qbittorrent', 'sabnzbd')),
    name     TEXT NOT NULL,
    url      TEXT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    password TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL DEFAULT 'monarr',
    enabled  INTEGER NOT NULL DEFAULT 1,
    added_at INTEGER NOT NULL
) STRICT;

CREATE TABLE downloads (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    -- JSON array of wantable ids this grab targets (a pack lists many).
    wantables     TEXT NOT NULL DEFAULT '[]',
    season        INTEGER NOT NULL DEFAULT -1,
    release_title TEXT NOT NULL,
    indexer       TEXT NOT NULL DEFAULT '',
    protocol      TEXT NOT NULL,
    quality       TEXT NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    client_id     INTEGER NOT NULL DEFAULT 0,
    handle        TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'grabbed'
        CHECK (state IN ('grabbed','downloading','completed','importing','imported','failed')),
    progress      REAL NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    added_at      INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_downloads_state ON downloads (state);

CREATE TABLE history_events (
    id            INTEGER PRIMARY KEY,
    ts            INTEGER NOT NULL,
    type          TEXT NOT NULL, -- grabbed | imported | failed
    media_item_id INTEGER NOT NULL DEFAULT 0,
    release_title TEXT NOT NULL DEFAULT '',
    data          TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX idx_history_ts ON history_events (ts DESC);
