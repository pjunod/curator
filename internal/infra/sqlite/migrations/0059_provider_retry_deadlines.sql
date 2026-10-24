-- +goose Up
ALTER TABLE downloads ADD COLUMN provider_retry_after INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_delivery_receipts ADD COLUMN provider_retry_after INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_delivery_source_refresh ADD COLUMN provider_retry_after INTEGER NOT NULL DEFAULT 0;

-- +goose Down
SELECT CASE WHEN EXISTS (SELECT 1 FROM downloads WHERE provider_retry_after != 0)
 OR EXISTS (SELECT 1 FROM runner_delivery_receipts WHERE provider_retry_after != 0)
 OR EXISTS (SELECT 1 FROM runner_delivery_source_refresh WHERE provider_retry_after != 0)
 THEN 0 ELSE 1 END;
ALTER TABLE runner_delivery_source_refresh DROP COLUMN provider_retry_after;
ALTER TABLE runner_delivery_receipts DROP COLUMN provider_retry_after;
ALTER TABLE downloads DROP COLUMN provider_retry_after;
