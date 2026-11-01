-- +goose Up
ALTER TABLE runner_delivery_receipts ADD COLUMN lifecycle_timing_json TEXT NOT NULL DEFAULT 'null';
ALTER TABLE runner_delivery_receipts ADD COLUMN runner_stages_json TEXT NOT NULL DEFAULT 'null';
ALTER TABLE runner_delivery_receipts ADD COLUMN source_copies_json TEXT NOT NULL DEFAULT 'null';

-- +goose Down
CREATE TEMP TABLE delivery_stage_downgrade_guard(ok INTEGER CHECK(ok=1));
INSERT INTO delivery_stage_downgrade_guard
 SELECT CASE WHEN EXISTS(SELECT 1 FROM runner_delivery_receipts WHERE runner_stages_json!='null' OR source_copies_json!='null' OR lifecycle_timing_json!='null') THEN 0 ELSE 1 END;
DROP TABLE delivery_stage_downgrade_guard;
ALTER TABLE runner_delivery_receipts DROP COLUMN source_copies_json;
ALTER TABLE runner_delivery_receipts DROP COLUMN runner_stages_json;
ALTER TABLE runner_delivery_receipts DROP COLUMN lifecycle_timing_json;
