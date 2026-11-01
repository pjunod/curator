-- +goose Up
ALTER TABLE runner_delivery_explicit_delete_batches ADD COLUMN abandoned_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_delivery_explicit_delete_files ADD COLUMN unlink_started INTEGER NOT NULL DEFAULT 0 CHECK(unlink_started IN (0,1));

-- +goose Down
ALTER TABLE runner_delivery_explicit_delete_files DROP COLUMN unlink_started;
ALTER TABLE runner_delivery_explicit_delete_batches DROP COLUMN abandoned_at;
