-- +goose Up
-- Phase 3 schema: the blocklist (failed releases never re-grabbed) and
-- notifier configurations. Timestamps are unix millis.

CREATE TABLE blocklist (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL DEFAULT 0,
    release_title TEXT NOT NULL,
    indexer       TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_blocklist_release ON blocklist (release_title, indexer);

CREATE TABLE notifiers (
    id        INTEGER PRIMARY KEY,
    type      TEXT NOT NULL CHECK (type IN ('webhook', 'discord', 'plex', 'jellyfin')),
    name      TEXT NOT NULL,
    -- JSON settings: {"url": "...", "token": "..."} — shape depends on type.
    settings  TEXT NOT NULL DEFAULT '{}',
    on_grab   INTEGER NOT NULL DEFAULT 1,
    on_import INTEGER NOT NULL DEFAULT 1,
    on_failed INTEGER NOT NULL DEFAULT 1,
    on_health INTEGER NOT NULL DEFAULT 0,
    enabled   INTEGER NOT NULL DEFAULT 1
) STRICT;
