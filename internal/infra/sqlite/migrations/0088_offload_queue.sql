-- +goose Up
DROP INDEX offload_operations_open;
DROP INDEX offload_operations_item;
ALTER TABLE offload_operations RENAME TO offload_operations_old;
CREATE TABLE offload_operations (
    operation_id  TEXT PRIMARY KEY,
    request_id    TEXT NOT NULL UNIQUE,
    fingerprint   TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('queued','deleting','waiting','retrying','stopping','complete','cancelled','failed','needs_review')),
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
CREATE INDEX offload_operations_open ON offload_operations(state) WHERE state NOT IN ('complete','cancelled','failed','needs_review');
CREATE INDEX offload_operations_item ON offload_operations(media_item_id, created_at DESC);
INSERT INTO offload_operations
SELECT operation_id, request_id, fingerprint, state, media_item_id, scope,
       plan, result, files_total, bytes_total, bytes_freed, last_error, created_at, updated_at, dismissed_at
FROM offload_operations_old ORDER BY rowid;
DROP TABLE offload_operations_old;

CREATE TABLE offload_request_aliases (
    request_id TEXT PRIMARY KEY,
    operation_id TEXT NOT NULL
) STRICT;

ALTER TABLE offload_operation_files ADD COLUMN unlink_started INTEGER NOT NULL DEFAULT 0 CHECK (unlink_started IN (0,1));

-- +goose Down
ALTER TABLE offload_operation_files DROP COLUMN unlink_started;
DROP TABLE offload_request_aliases;
DROP INDEX offload_operations_open;
DROP INDEX offload_operations_item;
ALTER TABLE offload_operations RENAME TO offload_operations_old;
CREATE TABLE offload_operations (
    operation_id  TEXT PRIMARY KEY,
    request_id    TEXT NOT NULL UNIQUE,
    fingerprint   TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('deleting','waiting','retrying','stopping','complete','cancelled','failed')),
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
INSERT INTO offload_operations
SELECT operation_id, request_id, fingerprint, CASE state WHEN 'queued' THEN 'cancelled' WHEN 'needs_review' THEN 'failed' ELSE state END, media_item_id, scope,
       plan, result, files_total, bytes_total, bytes_freed, last_error, created_at, updated_at, dismissed_at
FROM offload_operations_old ORDER BY rowid;
DROP TABLE offload_operations_old;
