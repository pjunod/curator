-- +goose Up
-- Offload (ADR 0028): free the space, keep the title.
--
-- 1. offloaded_at (unix ms, 0 = not offloaded) lives on the rows that hold
--    `monitored`: the movie's media_items row, a season, an episode. It is its
--    own state, not "unmonitored": the one Mon computation folds it in, so
--    RSS, backlog, upgrades and Auto Search skip an offloaded wantable with
--    no check of their own. `monitored` is left as the person set it.
-- 2. Triggers keep the state honest in the store, for every path that can
--    attach a file (publication import, manual import, scan adoption, Re-match
--    move-in, interactive grab): a file row attached to an offloaded movie, or
--    linked to an offloaded episode, clears it (and a season's flag once no
--    episode of it is still offloaded). A new episode inside an offloaded
--    season inherits the season's state.
-- 3. offload_operations is the durable operation (request id + fingerprint
--    like a Re-match, state, the plan it executes, what was freed) and
--    offload_operation_files its per-file journal (media files, their
--    sidecars and untracked subtitles), so a restart resumes mid-delete.
-- 4. notifiers' features already exist (0079); offload rides in them.
ALTER TABLE media_items ADD COLUMN offloaded_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE seasons ADD COLUMN offloaded_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE episodes ADD COLUMN offloaded_at INTEGER NOT NULL DEFAULT 0;

-- +goose StatementBegin
CREATE TRIGGER media_files_clear_offloaded_insert AFTER INSERT ON media_files
BEGIN
    UPDATE media_items SET offloaded_at = 0 WHERE id = NEW.media_item_id AND offloaded_at != 0;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER media_files_clear_offloaded_move AFTER UPDATE OF media_item_id ON media_files
BEGIN
    UPDATE media_items SET offloaded_at = 0 WHERE id = NEW.media_item_id AND offloaded_at != 0;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER media_file_episodes_clear_offloaded AFTER INSERT ON media_file_episodes
BEGIN
    UPDATE episodes SET offloaded_at = 0 WHERE id = NEW.episode_id AND offloaded_at != 0;
    UPDATE seasons SET offloaded_at = 0
     WHERE offloaded_at != 0
       AND (media_item_id, number) IN (SELECT media_item_id, season_number FROM episodes WHERE id = NEW.episode_id)
       AND NOT EXISTS (SELECT 1 FROM episodes e WHERE e.media_item_id = seasons.media_item_id AND e.season_number = seasons.number AND e.offloaded_at != 0);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER episodes_inherit_offloaded AFTER INSERT ON episodes
WHEN NEW.offloaded_at = 0
BEGIN
    UPDATE episodes SET offloaded_at = (SELECT s.offloaded_at FROM seasons s WHERE s.media_item_id = NEW.media_item_id AND s.number = NEW.season_number)
     WHERE id = NEW.id AND EXISTS (SELECT 1 FROM seasons s WHERE s.media_item_id = NEW.media_item_id AND s.number = NEW.season_number AND s.offloaded_at != 0);
END;
-- +goose StatementEnd

CREATE TABLE offload_operations (
    operation_id  TEXT PRIMARY KEY,
    request_id    TEXT NOT NULL UNIQUE,
    fingerprint   TEXT NOT NULL,
    -- deleting: the files are being deleted; waiting: the share is not
    -- listable (nothing was marked deleted); retrying: a delete failed and
    -- is retried; complete; cancelled (Stop); failed (unreadable plan).
    state         TEXT NOT NULL CHECK (state IN ('deleting','waiting','retrying','complete','cancelled','failed')),
    media_item_id INTEGER NOT NULL,
    scope         TEXT NOT NULL CHECK (scope IN ('item','season','episode')),
    plan          TEXT NOT NULL DEFAULT '{}',
    result        TEXT NOT NULL DEFAULT '{}',
    files_total   INTEGER NOT NULL DEFAULT 0,
    bytes_total   INTEGER NOT NULL DEFAULT 0,
    bytes_freed   INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    dismissed_at  INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX offload_operations_open ON offload_operations(state) WHERE state NOT IN ('complete','cancelled','failed');
CREATE INDEX offload_operations_item ON offload_operations(media_item_id, created_at DESC);

CREATE TABLE offload_operation_files (
    operation_id TEXT NOT NULL,
    path         TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('media','sidecar')),
    file_id      INTEGER NOT NULL DEFAULT 0,
    size         INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL CHECK (state IN ('pending','deleted','failed')),
    error        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (operation_id, path)
) STRICT, WITHOUT ROWID;

-- +goose Down
DROP TABLE offload_operation_files;
DROP TABLE offload_operations;
DROP TRIGGER episodes_inherit_offloaded;
DROP TRIGGER media_file_episodes_clear_offloaded;
DROP TRIGGER media_files_clear_offloaded_move;
DROP TRIGGER media_files_clear_offloaded_insert;
ALTER TABLE episodes DROP COLUMN offloaded_at;
ALTER TABLE seasons DROP COLUMN offloaded_at;
ALTER TABLE media_items DROP COLUMN offloaded_at;
