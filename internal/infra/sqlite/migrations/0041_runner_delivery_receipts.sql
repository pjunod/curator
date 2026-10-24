-- +goose Up
-- Runner publications need an independent, restart-safe Curator acceptance
-- journal. Activity rows may be cleared without forgetting custody or the
-- idempotent acknowledgement that Runner is waiting for.
CREATE TABLE runner_delivery_receipts (
    id                      INTEGER PRIMARY KEY,
    client_id               INTEGER NOT NULL,
    delivery_id             TEXT NOT NULL,
    sequence                INTEGER NOT NULL,
    publication_generation  TEXT NOT NULL,
    manifest_digest         TEXT NOT NULL,
    artifact_id             TEXT NOT NULL,
    job_id                  TEXT NOT NULL,
    request_token           TEXT NOT NULL DEFAULT '',
    runner_instance_id      TEXT NOT NULL DEFAULT '',
    ready_at                INTEGER NOT NULL,
    publication_path        TEXT NOT NULL,
    mapped_path             TEXT NOT NULL DEFAULT '',
    files_json              TEXT NOT NULL,
    source_cleanup_json     TEXT NOT NULL DEFAULT 'null',
    state                   TEXT NOT NULL DEFAULT 'unresolved'
        CHECK (state IN ('unresolved','ready','importing','imported','conflict')),
    ack_state               TEXT NOT NULL DEFAULT 'not_accepted'
        CHECK (ack_state IN ('not_accepted','pending','accepted')),
    consumer_receipt_id     TEXT NOT NULL DEFAULT '',
    download_id             INTEGER NOT NULL DEFAULT 0,
    generation_key          TEXT NOT NULL DEFAULT '',
    download_handle         TEXT NOT NULL DEFAULT '',
    media_item_id           INTEGER NOT NULL DEFAULT 0,
    copy_id                 INTEGER NOT NULL DEFAULT 0,
    error                   TEXT NOT NULL DEFAULT '',
    attempts                INTEGER NOT NULL DEFAULT 0,
    blocked_since           INTEGER NOT NULL DEFAULT 0,
    next_attempt_at         INTEGER NOT NULL DEFAULT 0,
    created_at              INTEGER NOT NULL,
    updated_at              INTEGER NOT NULL,
    UNIQUE (client_id, delivery_id)
) STRICT;

CREATE INDEX idx_runner_delivery_receipts_work
    ON runner_delivery_receipts (client_id, state, ack_state, next_attempt_at, sequence);

CREATE TABLE runner_delivery_cursors (
    client_id       INTEGER PRIMARY KEY,
    after_sequence  INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL
) STRICT;

-- A configured authenticated client is pinned to the Runner installation
-- identity it first observed. Repointing that client cannot silently bind a
-- reused job id to another installation.
CREATE TABLE runner_client_instances (
    client_id         INTEGER PRIMARY KEY,
    runner_instance_id TEXT NOT NULL,
    delivery_v1_enabled INTEGER NOT NULL DEFAULT 0 CHECK(delivery_v1_enabled IN (0,1)),
    updated_at        INTEGER NOT NULL
) STRICT;

-- +goose StatementBegin
CREATE TRIGGER prevent_download_delete_with_runner_delivery
BEFORE DELETE ON downloads
WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts r
    WHERE r.download_id=OLD.id AND (r.state!='imported' OR r.ack_state!='accepted'))
BEGIN
    SELECT RAISE(ABORT, 'download has unresolved Runner delivery custody');
END;
-- +goose StatementEnd
