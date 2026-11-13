-- +goose NO TRANSACTION
-- +goose Up
-- Re-match (ADR 0027): move files to the right item or episode.
--
-- 1. media_items.origin says who created an item: 'user' (added by a person,
--    an import list, discovery) or 'adopted' (folder adoption created it to
--    hold a folder it found). Only an adopted item that a Re-match empties is
--    removed; every existing row is 'user', so a legacy item is never removed
--    automatically.
-- 2. replacement_operations gains the 'rematch' intent: the release behind a
--    moved file is blocklisted for the item it left through the replacement
--    path, as an operation recorded already complete (no search, no removal).
--    The intent CHECK lives on the table, so it is rebuilt. Two tables
--    reference it ON DELETE RESTRICT, and with foreign keys on a DROP TABLE
--    runs that check as an implicit DELETE; PRAGMA foreign_keys is a no-op in
--    a transaction, so this file manages its own.
-- 3. rematch_operations is the operation record: request id + fingerprint
--    (idempotent like replacement creation), state, the plan it executes and
--    the per-file result (states planned → moving → moved → committed →
--    complete, or cancel_pending → cancelled | failed). Its file moves are
--    journaled in import_placements
--    (kind rematch_move, states rematch_*), which the import reconciler never
--    reads.
-- 4. notifier_deliveries.operation_id keys a sub-delivery by operation
--    (op/<operation-id>/<dir-hash>, contract amendment 4.1) instead of by
--    publication.
-- 5. notifiers.features is what Cinema's connection test said it supports
--    (["removed_paths"]), stored at Test and re-read before a session's first
--    moved send; features_at is when they were last read (0 = never).
PRAGMA foreign_keys = OFF;

-- +goose StatementBegin
BEGIN;

ALTER TABLE media_items ADD COLUMN origin TEXT NOT NULL DEFAULT 'user' CHECK (origin IN ('user', 'adopted'));

CREATE TABLE replacement_operations_new (
    operation_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    fingerprint TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('accepted','removing','cleanup_blocked','searching','waiting_for_release','replacing','complete','cancelled','failed')),
    intent TEXT NOT NULL CHECK (intent IN ('replace_bad','replace_quality','rematch')),
    scope TEXT NOT NULL CHECK (scope IN ('episodes','download')),
    media_item_id INTEGER NOT NULL,
    copy_id INTEGER NOT NULL DEFAULT 0,
    source_generation TEXT NOT NULL,
    source_transfer TEXT NOT NULL DEFAULT '',
    source_release TEXT NOT NULL DEFAULT '',
    source_indexer TEXT NOT NULL DEFAULT '',
    source_guid TEXT NOT NULL DEFAULT '',
    source_indexer_id INTEGER NOT NULL DEFAULT 0,
    blacklist_source_release INTEGER NOT NULL CHECK (blacklist_source_release IN (0,1)),
    reason_code TEXT NOT NULL,
    reason TEXT NOT NULL,
    episode_ids TEXT NOT NULL DEFAULT '[]',
    result TEXT NOT NULL DEFAULT '{}',
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    retry_attempts INTEGER NOT NULL DEFAULT 0,
    blocked_since INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO replacement_operations_new SELECT operation_id,request_id,fingerprint,state,intent,scope,media_item_id,copy_id,source_generation,source_transfer,source_release,source_indexer,source_guid,source_indexer_id,blacklist_source_release,reason_code,reason,episode_ids,result,last_error,created_at,updated_at,retry_attempts,blocked_since,next_attempt_at FROM replacement_operations;
DROP TABLE replacement_operations;
ALTER TABLE replacement_operations_new RENAME TO replacement_operations;
CREATE INDEX replacement_operation_state ON replacement_operations(state, updated_at);

-- The rebuild must leave every child row pointing at its operation: a
-- violation aborts the migration rather than leaving it to a later DELETE.
CREATE TEMP TABLE fk_guard_0079 (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO fk_guard_0079 SELECT CASE WHEN EXISTS (SELECT 1 FROM pragma_foreign_key_check('replacement_operation_files'))
    OR EXISTS (SELECT 1 FROM pragma_foreign_key_check('replacement_operation_steps')) THEN 0 ELSE 1 END;
DROP TABLE fk_guard_0079;

CREATE TABLE rematch_operations (
    operation_id   TEXT PRIMARY KEY,
    request_id     TEXT NOT NULL UNIQUE,
    fingerprint    TEXT NOT NULL,
    state          TEXT NOT NULL CHECK (state IN ('planned','moving','moved','committed','complete','cancel_pending','cancelled','failed')),
    source_item_id INTEGER NOT NULL,
    target_item_id INTEGER NOT NULL,
    plan           TEXT NOT NULL DEFAULT '{}',
    result         TEXT NOT NULL DEFAULT '{}',
    bytes_done     INTEGER NOT NULL DEFAULT 0,
    bytes_total    INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;
CREATE INDEX rematch_operations_open ON rematch_operations(state) WHERE state NOT IN ('complete','cancelled','failed');
CREATE INDEX rematch_operations_source ON rematch_operations(source_item_id, created_at DESC);
CREATE INDEX rematch_operations_target ON rematch_operations(target_item_id, created_at DESC);

ALTER TABLE notifier_deliveries ADD COLUMN operation_id TEXT NOT NULL DEFAULT '';
-- A moved request was posted with removed_paths (1) or as the new folder
-- only (0); a later read of the request is judged by what was sent.
ALTER TABLE notifier_deliveries ADD COLUMN removed_sent INTEGER NOT NULL DEFAULT 0;
-- An operation's journaled moves, read by operation.
CREATE INDEX import_placements_rematch ON import_placements(json_extract(data,'$.operation_id')) WHERE json_extract(data,'$.kind') = 'rematch_move';
CREATE INDEX notifier_deliveries_operation ON notifier_deliveries(operation_id) WHERE operation_id != '';

ALTER TABLE notifiers ADD COLUMN features TEXT NOT NULL DEFAULT '[]';
ALTER TABLE notifiers ADD COLUMN features_at INTEGER NOT NULL DEFAULT 0;

COMMIT;
-- +goose StatementEnd

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

-- +goose StatementBegin
BEGIN;

DELETE FROM import_placements WHERE json_extract(data,'$.kind') = 'rematch_move';
DROP INDEX import_placements_rematch;
DELETE FROM notifier_deliveries WHERE operation_id != '';
ALTER TABLE notifier_deliveries DROP COLUMN removed_sent;
ALTER TABLE notifiers DROP COLUMN features_at;
ALTER TABLE notifiers DROP COLUMN features;
DROP INDEX notifier_deliveries_operation;
ALTER TABLE notifier_deliveries DROP COLUMN operation_id;
DROP TABLE rematch_operations;

DELETE FROM replacement_operation_steps WHERE operation_id IN (SELECT operation_id FROM replacement_operations WHERE intent = 'rematch');
DELETE FROM replacement_operation_files WHERE operation_id IN (SELECT operation_id FROM replacement_operations WHERE intent = 'rematch');
CREATE TABLE replacement_operations_old (
    operation_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    fingerprint TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('accepted','removing','cleanup_blocked','searching','waiting_for_release','replacing','complete','cancelled','failed')),
    intent TEXT NOT NULL CHECK (intent IN ('replace_bad','replace_quality')),
    scope TEXT NOT NULL CHECK (scope IN ('episodes','download')),
    media_item_id INTEGER NOT NULL,
    copy_id INTEGER NOT NULL DEFAULT 0,
    source_generation TEXT NOT NULL,
    source_transfer TEXT NOT NULL DEFAULT '',
    source_release TEXT NOT NULL DEFAULT '',
    source_indexer TEXT NOT NULL DEFAULT '',
    source_guid TEXT NOT NULL DEFAULT '',
    source_indexer_id INTEGER NOT NULL DEFAULT 0,
    blacklist_source_release INTEGER NOT NULL CHECK (blacklist_source_release IN (0,1)),
    reason_code TEXT NOT NULL,
    reason TEXT NOT NULL,
    episode_ids TEXT NOT NULL DEFAULT '[]',
    result TEXT NOT NULL DEFAULT '{}',
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    retry_attempts INTEGER NOT NULL DEFAULT 0,
    blocked_since INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO replacement_operations_old SELECT * FROM replacement_operations WHERE intent != 'rematch';
DROP TABLE replacement_operations;
ALTER TABLE replacement_operations_old RENAME TO replacement_operations;
CREATE INDEX replacement_operation_state ON replacement_operations(state, updated_at);

ALTER TABLE media_items DROP COLUMN origin;

COMMIT;
-- +goose StatementEnd

PRAGMA foreign_keys = ON;
