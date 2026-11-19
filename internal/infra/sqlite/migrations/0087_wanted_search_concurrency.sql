-- +goose Up
DROP INDEX wanted_search_one_active;
CREATE UNIQUE INDEX wanted_search_active_scope
    ON wanted_search_runs (scope, reason, media_item_id, wantable_id, target_delay_ms)
    WHERE status IN ('queued', 'running') AND cancel_requested_at = 0;

CREATE TABLE wanted_search_requests (
    request_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES wanted_search_runs(run_id) ON DELETE CASCADE
);
CREATE INDEX wanted_search_requests_run ON wanted_search_requests(run_id);

-- +goose Down
DROP TABLE wanted_search_requests;
DROP INDEX wanted_search_active_scope;
CREATE UNIQUE INDEX wanted_search_one_active
    ON wanted_search_runs ((1))
    WHERE status IN ('queued', 'running');
