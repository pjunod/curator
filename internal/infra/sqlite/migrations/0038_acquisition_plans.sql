-- +goose Up
CREATE TABLE acquisition_plans (
 id INTEGER PRIMARY KEY,
 media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
 copy_id INTEGER NOT NULL DEFAULT 0,
 season INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('admitted','dispatching','active','cancel_requested','completed','cancelled','needs_replan','settled_with_reservations')),
 revision INTEGER NOT NULL DEFAULT 1,
 policy_version INTEGER NOT NULL DEFAULT 1,
 snapshot_fingerprint TEXT NOT NULL,
 decision TEXT NOT NULL,
 replan_pending INTEGER NOT NULL DEFAULT 0,
 added_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX acquisition_plan_scope ON acquisition_plans(media_item_id,copy_id,season)
 WHERE state IN ('admitted','dispatching','active','cancel_requested');
CREATE TABLE downloads_new (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    copy_id       INTEGER,
    wantables     TEXT NOT NULL DEFAULT '[]',
    season        INTEGER NOT NULL DEFAULT -1,
    release_title TEXT NOT NULL,
    indexer       TEXT NOT NULL DEFAULT '',
    protocol      TEXT NOT NULL,
    quality       TEXT NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    client_id     INTEGER NOT NULL DEFAULT 0,
    handle        TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'grabbed'
        CHECK (state IN ('planned','grabbed','downloading','downloaded','awaiting_import','importing','imported','failed')),
    progress      REAL NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    save_path     TEXT NOT NULL DEFAULT '',
    import_path   TEXT NOT NULL DEFAULT '',
    handoff_log   TEXT NOT NULL DEFAULT '[]',
    added_at      INTEGER NOT NULL,
    transfer TEXT NOT NULL DEFAULT '',
    payload_removed INTEGER NOT NULL DEFAULT 0,
    match_evidence TEXT NOT NULL DEFAULT '{}',
    runner_control TEXT NOT NULL DEFAULT '',
    plan_id INTEGER REFERENCES acquisition_plans(id),
    candidate_key TEXT NOT NULL DEFAULT '',
    submission_phase TEXT NOT NULL DEFAULT 'submitted' CHECK(submission_phase IN ('pending','submitting','submitted','uncertain','rejected')),
    execution_payload TEXT NOT NULL DEFAULT '{}',
    reserved_episodes TEXT NOT NULL DEFAULT '[]',
    parked_reason TEXT NOT NULL DEFAULT '',
    parked_at INTEGER NOT NULL DEFAULT 0,
    superseded INTEGER NOT NULL DEFAULT 0,
    observation TEXT NOT NULL DEFAULT '{}',
    cleanup_pending INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL
) STRICT;

INSERT INTO downloads_new (id,media_item_id,copy_id,wantables,season,release_title,indexer,protocol,quality,size,client_id,handle,state,progress,error,save_path,import_path,handoff_log,added_at,updated_at,transfer,payload_removed,match_evidence,runner_control) SELECT id,media_item_id,copy_id,wantables,season,release_title,indexer,protocol,quality,size,client_id,handle,state,progress,error,save_path,import_path,handoff_log,added_at,updated_at,transfer,payload_removed,match_evidence,runner_control FROM downloads;
DROP TABLE downloads;
ALTER TABLE downloads_new RENAME TO downloads;

CREATE INDEX idx_downloads_state ON downloads(state);
CREATE UNIQUE INDEX download_plan_candidate ON downloads(plan_id,candidate_key) WHERE plan_id IS NOT NULL;
CREATE INDEX download_failure_window ON downloads(state,added_at);
ALTER TABLE indexers ADD COLUMN daily_request_cap INTEGER NOT NULL DEFAULT 0 CHECK(daily_request_cap>=0);
ALTER TABLE indexers ADD COLUMN next_rss_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE indexers ADD COLUMN retry_at INTEGER NOT NULL DEFAULT 0;
CREATE TABLE indexer_request_usage (
 indexer_id INTEGER NOT NULL REFERENCES indexers(id) ON DELETE CASCADE,
 requested_at INTEGER NOT NULL,
 bucket TEXT NOT NULL CHECK(bucket IN ('rss','search','interactive','headroom')),
 units INTEGER NOT NULL DEFAULT 1 CHECK(units>0),
 automatic INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX indexer_usage_window ON indexer_request_usage(indexer_id,requested_at);
-- The policy snapshot must also fence transport/configuration and format changes.
-- +goose StatementBegin
CREATE TRIGGER acquisition_indexers_insert AFTER INSERT ON indexers
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_indexers_update AFTER UPDATE ON indexers
WHEN OLD.url!=NEW.url OR OLD.api_key!=NEW.api_key OR OLD.categories!=NEW.categories OR OLD.protocol!=NEW.protocol OR OLD.enabled!=NEW.enabled OR OLD.daily_request_cap!=NEW.daily_request_cap
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_indexers_delete AFTER DELETE ON indexers
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_insert AFTER INSERT ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_update AFTER UPDATE ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_delete AFTER DELETE ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_custom_formats_insert AFTER INSERT ON custom_formats
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_custom_formats_update AFTER UPDATE ON custom_formats
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_custom_formats_delete AFTER DELETE ON custom_formats
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd

-- +goose Down
-- Refuse destructive downgrade while acquisition evidence exists.
-- +goose StatementBegin
CREATE TEMP TABLE acquisition_downgrade_guard(n INTEGER CHECK(n=0));
INSERT INTO acquisition_downgrade_guard SELECT count(*) FROM acquisition_plans;
DROP TABLE acquisition_downgrade_guard;
-- +goose StatementEnd
