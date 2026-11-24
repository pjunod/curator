-- +goose Up
-- Pipeline contract v2, M5 (docs/PIPELINE_CONTRACT.md section 4): Cinema is
-- told through a durable outbox, one sub-delivery per (notifier, publication,
-- directory), and the answer is read back.
--
-- A sub-delivery row is written in the publication's completing transaction
-- (no bus hop, nothing to drop). Its idempotency key
-- <transfer>/<generation>/<dir-hash> is stable for the life of the row; the
-- request Cinema sees is <key>/<attempt>. The attempt is reused after an
-- ambiguous outcome (a timeout, a connection error) so a lost response does
-- not create a second scan request, and advanced after a confirmed retryable
-- result (an expected file not placed, a 503 path_unreadable, a failed
-- request) so the retry gets a fresh scan instead of the cached answer.
--
--   expected_files_json  [{path, bytes, mtime_ns}] the media files this import
--                        placed under `directory`, stat'ed after placement
--   remote_request_id    Cinema's request id after a 202, polled on the schedule
--   remote_state         queued | running | done | failed, as Cinema reports it
--   waiting_on           Cinema's waiting_on while queued (full_scan:<lib>, ...)
--   visible_at           when every expected file came back placed with
--                        matching bytes and mtime_ns (0 = not visible)
--   item_ids_json        the Cinema item ids it reported for this directory
--
-- `status` keeps its CHECK (pending | ok | failed). "warning" is derived by the
-- API: a sub-delivery still pending 30 minutes after it was written.
-- Chat-shaped notifiers' import rows carry no key and no directory.
ALTER TABLE notifier_deliveries ADD COLUMN publication_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notifier_deliveries ADD COLUMN directory TEXT NOT NULL DEFAULT '';
ALTER TABLE notifier_deliveries ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT '';
ALTER TABLE notifier_deliveries ADD COLUMN attempt INTEGER NOT NULL DEFAULT 1;
ALTER TABLE notifier_deliveries ADD COLUMN expected_files_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE notifier_deliveries ADD COLUMN remote_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE notifier_deliveries ADD COLUMN remote_state TEXT NOT NULL DEFAULT '';
ALTER TABLE notifier_deliveries ADD COLUMN waiting_on TEXT NOT NULL DEFAULT '';
ALTER TABLE notifier_deliveries ADD COLUMN visible_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE notifier_deliveries ADD COLUMN item_ids_json TEXT NOT NULL DEFAULT '[]';
CREATE UNIQUE INDEX notifier_deliveries_idem ON notifier_deliveries (notifier_id, idempotency_key) WHERE idempotency_key != '';

-- +goose Down
DROP INDEX notifier_deliveries_idem;
ALTER TABLE notifier_deliveries DROP COLUMN item_ids_json;
ALTER TABLE notifier_deliveries DROP COLUMN visible_at;
ALTER TABLE notifier_deliveries DROP COLUMN waiting_on;
ALTER TABLE notifier_deliveries DROP COLUMN remote_state;
ALTER TABLE notifier_deliveries DROP COLUMN remote_request_id;
ALTER TABLE notifier_deliveries DROP COLUMN expected_files_json;
ALTER TABLE notifier_deliveries DROP COLUMN attempt;
ALTER TABLE notifier_deliveries DROP COLUMN idempotency_key;
ALTER TABLE notifier_deliveries DROP COLUMN directory;
ALTER TABLE notifier_deliveries DROP COLUMN publication_id;
