-- +goose Up
-- A reviewed multi-file explicit deletion is one durable selection. The
-- journal survives partial unlink, Activity removal, and a changed inventory
-- fingerprint without authorizing additional paths on replay.
CREATE TABLE runner_delivery_explicit_delete_batches (
    batch_id TEXT PRIMARY KEY,
    receipt_id INTEGER NOT NULL REFERENCES runner_delivery_receipts(id),
    payload_path TEXT NOT NULL,
    inventory_fingerprint TEXT NOT NULL,
    publication_directory_identity TEXT NOT NULL,
    storage_identity TEXT NOT NULL,
    generation_key TEXT NOT NULL,
    request_token TEXT NOT NULL,
    publication_generation TEXT NOT NULL,
    retry_attempts INTEGER NOT NULL DEFAULT 0,
    retry_next_attempt_at INTEGER NOT NULL DEFAULT 0,
    retry_blocked_since INTEGER NOT NULL DEFAULT 0,
    retry_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE(receipt_id, payload_path, inventory_fingerprint, batch_id)
) STRICT;

CREATE TABLE runner_delivery_explicit_delete_files (
    batch_id TEXT NOT NULL REFERENCES runner_delivery_explicit_delete_batches(batch_id),
    path TEXT NOT NULL,
    reviewed_path TEXT NOT NULL,
    snapshot_identity TEXT NOT NULL,
    parent_identity TEXT NOT NULL,
    history_recorded INTEGER NOT NULL DEFAULT 0 CHECK (history_recorded IN (0,1)),
    completed_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(batch_id, path)
) STRICT;

CREATE INDEX idx_delivery_explicit_delete_receipt
    ON runner_delivery_explicit_delete_batches(receipt_id, retry_next_attempt_at, created_at);

-- +goose Down
CREATE TEMP TABLE explicit_delivery_delete_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO explicit_delivery_delete_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_delivery_explicit_delete_batches)
    THEN 0 ELSE 1 END;
DROP INDEX idx_delivery_explicit_delete_receipt;
DROP TABLE runner_delivery_explicit_delete_files;
DROP TABLE runner_delivery_explicit_delete_batches;
DROP TABLE explicit_delivery_delete_downgrade_guard;
