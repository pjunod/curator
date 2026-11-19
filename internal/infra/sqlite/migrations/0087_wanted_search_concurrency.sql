-- +goose Up
DROP INDEX wanted_search_one_active;
CREATE UNIQUE INDEX wanted_search_active_scope
    ON wanted_search_runs (scope, reason, media_item_id, wantable_id, target_delay_ms)
    WHERE status IN ('queued', 'running') AND cancel_requested_at = 0;

-- +goose Down
DROP INDEX wanted_search_active_scope;
CREATE UNIQUE INDEX wanted_search_one_active
    ON wanted_search_runs ((1))
    WHERE status IN ('queued', 'running');
