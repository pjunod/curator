-- +goose Up
-- Per-client opt-in: hold a completed download at 'awaiting_import' until
-- the user approves it, instead of importing automatically. Off by default,
-- so existing clients keep importing hands-free. A plain scalar column (no
-- table rebuild needed), mirroring how 'enabled' is stored.

ALTER TABLE download_clients ADD COLUMN manual_approval INTEGER NOT NULL DEFAULT 0;
