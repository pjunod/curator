-- +goose Up
-- Curator-owned attempts are append-only and scoped to the durable delivery
-- receipt. A missing finish timestamp after a crash remains explicitly
-- unresolved; callers must not infer that work completed.
CREATE TABLE runner_delivery_attempt_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    receipt_id INTEGER NOT NULL REFERENCES runner_delivery_receipts(id) ON DELETE CASCADE,
    stage TEXT NOT NULL,
    attempt INTEGER NOT NULL CHECK (attempt > 0),
    started_at INTEGER NOT NULL,
    finished_at INTEGER,
    result TEXT NOT NULL DEFAULT 'running',
    error TEXT NOT NULL DEFAULT '',
    UNIQUE (receipt_id, stage, attempt)
);
CREATE INDEX runner_delivery_attempt_events_receipt ON runner_delivery_attempt_events(receipt_id, id);

-- +goose Down
CREATE TEMP TABLE delivery_attempt_events_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO delivery_attempt_events_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_delivery_attempt_events) THEN 0 ELSE 1 END;
DROP TABLE delivery_attempt_events_downgrade_guard;
DROP INDEX runner_delivery_attempt_events_receipt;
DROP TABLE runner_delivery_attempt_events;
