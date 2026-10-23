-- +goose Up
-- Keep Runner's mutable source-cleanup refresh independent from acceptance and
-- imports so a Runner outage cannot stall control-plane work.
CREATE TABLE runner_delivery_source_refresh (
    receipt_id INTEGER PRIMARY KEY REFERENCES runner_delivery_receipts(id) ON DELETE CASCADE,
    attempts INTEGER NOT NULL DEFAULT 0,
    blocked_since INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
) STRICT;

ALTER TABLE runner_delivery_receipts ADD COLUMN source_artifact_id TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_delivery_receipts ADD COLUMN source_generation TEXT NOT NULL DEFAULT '';

-- +goose Down
CREATE TEMP TABLE delivery_source_refresh_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO delivery_source_refresh_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts)
    OR EXISTS (SELECT 1 FROM runner_delivery_source_refresh WHERE attempts>0 OR last_error!='')
    THEN 0 ELSE 1 END;
DROP TABLE delivery_source_refresh_downgrade_guard;
DROP TABLE runner_delivery_source_refresh;
ALTER TABLE runner_delivery_receipts DROP COLUMN source_generation;
ALTER TABLE runner_delivery_receipts DROP COLUMN source_artifact_id;
