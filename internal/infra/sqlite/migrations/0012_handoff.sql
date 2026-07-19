-- +goose Up
-- Make the download → import "handoff" explicit and inspectable. The old
-- downloads table went completed → importing → imported in a single poll
-- tick with nothing persisted in between, so a completed-but-unimported
-- download was a black box. This:
--   * widens the state set with 'downloaded' (client finished, payload on
--     disk) and 'awaiting_import' (held for manual approval);
--   * records save_path (what the client reported) and import_path (where
--     Monarr looked after remote path mapping) so a stuck import shows the
--     exact path it checked;
--   * keeps handoff_log, a JSON array of {step, at, detail} entries, so
--     every step of the handoff is laid out with timestamps.
-- SQLite can't alter a CHECK, so the table is rebuilt (client_id is a plain
-- integer with no FK, so ids survive the copy — same as migration 0007).

CREATE TABLE downloads_new (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    copy_id       INTEGER,
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
        CHECK (state IN ('grabbed','downloading','downloaded','awaiting_import','importing','imported','failed')),
    progress      REAL NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    save_path     TEXT NOT NULL DEFAULT '',
    import_path   TEXT NOT NULL DEFAULT '',
    handoff_log   TEXT NOT NULL DEFAULT '[]',
    added_at      INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
) STRICT;

INSERT INTO downloads_new (
    id, media_item_id, copy_id, wantables, season, release_title, indexer,
    protocol, quality, size, client_id, handle, state, progress, error,
    added_at, updated_at)
SELECT
    id, media_item_id, copy_id, wantables, season, release_title, indexer,
    protocol, quality, size, client_id, handle,
    CASE state WHEN 'completed' THEN 'downloaded' ELSE state END,
    progress, error, added_at, updated_at
FROM downloads;

DROP TABLE downloads;
ALTER TABLE downloads_new RENAME TO downloads;
CREATE INDEX idx_downloads_state ON downloads (state);
