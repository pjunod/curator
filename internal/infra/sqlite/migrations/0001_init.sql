-- +goose Up
-- Phase 0 schema: app metadata + scheduler task state.
-- Policy (ADR 0004): migrations are up-only; we roll forward. This also lets
-- sqlc consume this directory directly as its schema source.
-- Timestamps are unix epoch milliseconds (INTEGER) throughout.

CREATE TABLE app_meta (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE scheduled_tasks (
    name             TEXT PRIMARY KEY,
    interval_seconds INTEGER NOT NULL,
    last_run_at      INTEGER,
    last_duration_ms INTEGER,
    last_error       TEXT,
    next_run_at      INTEGER,
    updated_at       INTEGER NOT NULL
) STRICT;
