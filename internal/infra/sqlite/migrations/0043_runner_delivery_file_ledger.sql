-- +goose Up
-- Per-file import rationale and completed-payload cleanup state are separate
-- durable facts. Paths are manifest-relative so duplicate basenames remain
-- distinct and the audit survives Activity dismissal.
CREATE TABLE runner_delivery_file_outcomes (
    receipt_id       INTEGER NOT NULL REFERENCES runner_delivery_receipts(id),
    path             TEXT NOT NULL,
    size_bytes       INTEGER NOT NULL,
    sha256           TEXT NOT NULL,
    file_generation  TEXT NOT NULL DEFAULT '',
    disposition      TEXT NOT NULL DEFAULT 'pending'
        CHECK (disposition IN ('pending','placed','redundant','disposable_selected_scope','unselected_recoverable','known_incidental','unknown_recoverable')),
    cleanup_state    TEXT NOT NULL DEFAULT 'protected'
        CHECK (cleanup_state IN ('protected','pending','removed','retry')),
    reason           TEXT NOT NULL DEFAULT '',
    library_path     TEXT NOT NULL DEFAULT '',
    manual_additional INTEGER NOT NULL DEFAULT 0 CHECK (manual_additional IN (0,1)),
    cleanup_error    TEXT NOT NULL DEFAULT '',
    cleanup_attempts INTEGER NOT NULL DEFAULT 0,
    cleanup_blocked_since INTEGER NOT NULL DEFAULT 0,
    cleanup_next_attempt_at INTEGER NOT NULL DEFAULT 0,
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (receipt_id, path)
) STRICT;

CREATE INDEX idx_runner_delivery_file_cleanup
    ON runner_delivery_file_outcomes (cleanup_state, receipt_id, path);

ALTER TABLE runner_delivery_receipts
    ADD COLUMN source_cleanup_refreshed_at INTEGER NOT NULL DEFAULT 0;

ALTER TABLE runner_delivery_receipts
    ADD COLUMN directory_cleanup_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_delivery_receipts
    ADD COLUMN directory_cleanup_blocked_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_delivery_receipts
    ADD COLUMN directory_cleanup_next_attempt_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_delivery_receipts
    ADD COLUMN directory_cleanup_state TEXT NOT NULL DEFAULT 'protected'
        CHECK (directory_cleanup_state IN ('protected','pending','removed','retry'));
ALTER TABLE runner_delivery_receipts
    ADD COLUMN directory_cleanup_error TEXT NOT NULL DEFAULT '';

-- Once local import commits, Activity may be cleared while the independent
-- idempotent Runner acknowledgement is retried from this receipt journal.
DROP TRIGGER prevent_download_delete_with_runner_delivery;
-- +goose StatementBegin
CREATE TRIGGER prevent_download_delete_with_runner_delivery
BEFORE DELETE ON downloads
WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts r
    WHERE r.download_id=OLD.id AND (r.state!='imported' OR r.ack_state!='accepted'))
BEGIN
    SELECT RAISE(ABORT, 'download has unresolved Runner delivery custody');
END;
-- +goose StatementEnd

-- +goose Down
-- The outcome ledger is the durable audit trail, including after all bytes
-- are gone. Do not downgrade while any receipt history remains.
CREATE TEMP TABLE runner_delivery_file_ledger_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO runner_delivery_file_ledger_downgrade_guard
    SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_delivery_file_outcomes)
        OR EXISTS (SELECT 1 FROM runner_delivery_receipts WHERE state!='imported' OR ack_state!='accepted')
        THEN 0 ELSE 1 END;
DROP TABLE runner_delivery_file_outcomes;
DROP TRIGGER prevent_download_delete_with_runner_delivery;
-- +goose StatementBegin
CREATE TRIGGER prevent_download_delete_with_runner_delivery
BEFORE DELETE ON downloads
WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts r
    WHERE r.download_id=OLD.id AND (r.state!='imported' OR r.ack_state!='accepted'))
BEGIN
    SELECT RAISE(ABORT, 'download has unresolved Runner delivery custody');
END;
-- +goose StatementEnd
ALTER TABLE runner_delivery_receipts DROP COLUMN source_cleanup_refreshed_at;
ALTER TABLE runner_delivery_receipts DROP COLUMN directory_cleanup_next_attempt_at;
ALTER TABLE runner_delivery_receipts DROP COLUMN directory_cleanup_blocked_since;
ALTER TABLE runner_delivery_receipts DROP COLUMN directory_cleanup_attempts;
ALTER TABLE runner_delivery_receipts DROP COLUMN directory_cleanup_error;
ALTER TABLE runner_delivery_receipts DROP COLUMN directory_cleanup_state;
DROP TABLE runner_delivery_file_ledger_downgrade_guard;
