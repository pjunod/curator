-- +goose Up
ALTER TABLE runner_delivery_receipts ADD COLUMN terminal_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_delivery_receipts ADD COLUMN failure_stage TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_delivery_receipts ADD COLUMN failure_cause TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_delivery_receipts ADD COLUMN keep_source_on_failure INTEGER NOT NULL DEFAULT 0 CHECK (keep_source_on_failure IN (0,1));
ALTER TABLE runner_delivery_receipts ADD COLUMN file_outcomes_json TEXT NOT NULL DEFAULT '[]';

-- +goose Down
CREATE TEMP TABLE runner_delivery_outcomes_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO runner_delivery_outcomes_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts) THEN 0 ELSE 1 END;
DROP TABLE runner_delivery_outcomes_downgrade_guard;
ALTER TABLE runner_delivery_receipts DROP COLUMN file_outcomes_json;
ALTER TABLE runner_delivery_receipts DROP COLUMN keep_source_on_failure;
ALTER TABLE runner_delivery_receipts DROP COLUMN failure_cause;
ALTER TABLE runner_delivery_receipts DROP COLUMN failure_stage;
ALTER TABLE runner_delivery_receipts DROP COLUMN terminal_outcome;
