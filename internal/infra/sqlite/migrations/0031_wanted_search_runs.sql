-- +goose Up
-- Explicit Wanted searches can outlive an HTTP request. Persist the accepted
-- snapshot, its cursor, and every result so restart and reload resume the same
-- operation rather than silently selecting a different wanted list.

CREATE TABLE wanted_search_runs (
    run_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL CHECK (scope IN ('all', 'reason', 'group', 'target')),
    reason TEXT NOT NULL DEFAULT '' CHECK (reason IN ('', 'missing', 'upgrade')),
    media_item_id INTEGER NOT NULL DEFAULT 0,
    wantable_id TEXT NOT NULL DEFAULT '',
    scope_label TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN (
        'queued', 'running', 'completed', 'failed', 'interrupted', 'cancelled'
    )),
    created_at INTEGER NOT NULL,
    started_at INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0,
    cancel_requested_at INTEGER NOT NULL DEFAULT 0,
    target_delay_ms INTEGER NOT NULL,
    selected INTEGER NOT NULL,
    processed INTEGER NOT NULL DEFAULT 0,
    searched INTEGER NOT NULL DEFAULT 0,
    skipped INTEGER NOT NULL DEFAULT 0,
    failed INTEGER NOT NULL DEFAULT 0,
    grabbed INTEGER NOT NULL DEFAULT 0,
    cursor INTEGER NOT NULL DEFAULT 0,
    next_ready_at INTEGER NOT NULL DEFAULT 0,
    job_id INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT ''
);

-- SQLite has no singleton table constraint. A constant expression plus a
-- partial unique index makes queued/running one atomic active slot.
CREATE UNIQUE INDEX wanted_search_one_active
    ON wanted_search_runs ((1))
    WHERE status IN ('queued', 'running');

CREATE TABLE wanted_search_targets (
    run_id TEXT NOT NULL REFERENCES wanted_search_runs(run_id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    wantable_id TEXT NOT NULL,
    selected_reason TEXT NOT NULL CHECK (selected_reason IN ('missing', 'upgrade')),
    label TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
        'pending', 'searched', 'skipped', 'failed'
    )),
    skipped TEXT NOT NULL DEFAULT '',
    seen INTEGER NOT NULL DEFAULT 0,
    matched INTEGER NOT NULL DEFAULT 0,
    accepted INTEGER NOT NULL DEFAULT 0,
    grabbed TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    finished_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, ordinal),
    UNIQUE (run_id, wantable_id)
);

CREATE INDEX wanted_search_targets_results
    ON wanted_search_targets (run_id, state, ordinal);

-- +goose Down
DROP TABLE wanted_search_targets;
DROP INDEX wanted_search_one_active;
DROP TABLE wanted_search_runs;
