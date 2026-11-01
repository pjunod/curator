-- +goose Up
ALTER TABLE runner_delivery_explicit_delete_batches ADD COLUMN abandoned_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_delivery_explicit_delete_files ADD COLUMN unlink_started INTEGER NOT NULL DEFAULT 0 CHECK(unlink_started IN (0,1));

-- +goose Down
-- Older binaries cannot represent cancellation and would revive remaining
-- reviewed selections. Preserve that authority boundary across downgrade.
CREATE TEMP TABLE explicit_delete_abandon_downgrade_guard(ok INTEGER CHECK(ok=1));
INSERT INTO explicit_delete_abandon_downgrade_guard
 SELECT CASE WHEN EXISTS(SELECT 1 FROM runner_delivery_explicit_delete_batches WHERE abandoned_at!=0) THEN 0 ELSE 1 END;
DROP TABLE explicit_delete_abandon_downgrade_guard;
ALTER TABLE runner_delivery_explicit_delete_files DROP COLUMN unlink_started;
ALTER TABLE runner_delivery_explicit_delete_batches DROP COLUMN abandoned_at;
