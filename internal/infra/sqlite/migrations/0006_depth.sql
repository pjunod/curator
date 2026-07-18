-- +goose Up
-- Phase 5 schema: custom formats and import lists.

CREATE TABLE custom_formats (
    id      INTEGER PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    pattern TEXT NOT NULL,
    score   INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE import_lists (
    id                 INTEGER PRIMARY KEY,
    name               TEXT NOT NULL,
    -- tmdb-popular | tmdb-top | trakt-list
    type               TEXT NOT NULL,
    -- JSON, type-specific: trakt-list {"user","slug","clientId"}
    config             TEXT NOT NULL DEFAULT '{}',
    kind               TEXT NOT NULL DEFAULT 'movie',
    root_folder_id     INTEGER NOT NULL DEFAULT 0,
    quality_profile_id INTEGER NOT NULL DEFAULT 1,
    monitored          INTEGER NOT NULL DEFAULT 1,
    enabled            INTEGER NOT NULL DEFAULT 1
) STRICT;
