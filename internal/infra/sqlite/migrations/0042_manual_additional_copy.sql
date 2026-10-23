-- +goose Up
-- Persist why a download was admitted. Existing rows remain automatic for
-- historical interpretation but are not Runner-manifest opt-ins.
ALTER TABLE downloads ADD COLUMN intent TEXT NOT NULL DEFAULT 'automatic'
    CHECK (intent IN ('automatic','manual_additional'));
ALTER TABLE downloads ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN request_fingerprint TEXT NOT NULL DEFAULT '';
ALTER TABLE downloads ADD COLUMN blacklist_override INTEGER NOT NULL DEFAULT 0
    CHECK (blacklist_override IN (0,1));
ALTER TABLE downloads ADD COLUMN delivery_expected INTEGER NOT NULL DEFAULT 0
    CHECK (delivery_expected IN (0,1));
ALTER TABLE downloads ADD COLUMN request_token_idempotent INTEGER NOT NULL DEFAULT 0
    CHECK (request_token_idempotent IN (0,1));
ALTER TABLE downloads ADD COLUMN manual_retry_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE downloads ADD COLUMN manual_retry_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE downloads ADD COLUMN manual_blocked_at INTEGER NOT NULL DEFAULT 0;

CREATE UNIQUE INDEX downloads_request_id ON downloads(request_id)
    WHERE request_id != '';

-- Request idempotency is independent of Activity/download-row retention. The
-- generation binding prevents a reused integer download ID from being
-- mistaken for the original manual request.
CREATE TABLE manual_grab_requests (
    request_id          TEXT PRIMARY KEY,
    request_fingerprint TEXT NOT NULL,
    download_id         INTEGER NOT NULL,
    generation_key      TEXT NOT NULL,
    copy_id             INTEGER NOT NULL,
    created_at          INTEGER NOT NULL
) STRICT;

-- +goose Down
-- Request idempotency cannot be discarded while any deliberate manual request
-- remains replayable.
-- +goose StatementBegin
CREATE TEMP TABLE manual_grab_downgrade_guard(n INTEGER CHECK(n=0));
INSERT INTO manual_grab_downgrade_guard SELECT count(*) FROM manual_grab_requests;
DROP TABLE manual_grab_downgrade_guard;
-- +goose StatementEnd
DROP TABLE manual_grab_requests;
DROP INDEX downloads_request_id;
ALTER TABLE downloads DROP COLUMN manual_blocked_at;
ALTER TABLE downloads DROP COLUMN manual_retry_attempts;
ALTER TABLE downloads DROP COLUMN manual_retry_at;
ALTER TABLE downloads DROP COLUMN request_token_idempotent;
ALTER TABLE downloads DROP COLUMN delivery_expected;
ALTER TABLE downloads DROP COLUMN blacklist_override;
ALTER TABLE downloads DROP COLUMN request_fingerprint;
ALTER TABLE downloads DROP COLUMN request_id;
ALTER TABLE downloads DROP COLUMN intent;
