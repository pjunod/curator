-- +goose Up
-- ADR 0008 step 1: a leased job queue, built on SQLite before any storage
-- change, because the design risk lives here and not in the store swap.
--
-- The point is that nothing in Monarr claims a unit of work today. The
-- scheduler's timers record what ran; they never claim anything, so a second
-- instance runs every task twice. A claim is what makes concurrency safe,
-- and it is worth having on one instance regardless: durable retries,
-- visible failures, and work that survives a restart.

CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    payload      TEXT NOT NULL DEFAULT '{}',  -- JSON, kind-specific
    state        TEXT NOT NULL DEFAULT 'queued'
                 CHECK (state IN ('queued', 'leased', 'done', 'failed')),
    priority     INTEGER NOT NULL DEFAULT 100, -- lower runs first
    run_after    INTEGER NOT NULL,             -- unix millis
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    last_error   TEXT NOT NULL DEFAULT '',

    -- Routing (ADR 0008 §2). For this workload the capability tags are
    -- mounts and toolchains rather than hardware: there is no transcode to
    -- place, so the only thing capability expresses is which node can see
    -- which files.
    dedupe_key          TEXT,
    required_capability TEXT,
    affinity_node       TEXT,

    -- Leasing.
    lease_owner      TEXT NOT NULL DEFAULT '',
    lease_expires_at INTEGER NOT NULL DEFAULT 0,

    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;

-- "Exactly one cluster-wide" without a leader: a singleton *job*, not a
-- singleton *node*. Any node may run it; the unique index means exactly one
-- does. Terminal rows are excluded so the next run can be enqueued.
CREATE UNIQUE INDEX idx_jobs_dedupe
    ON jobs (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND state IN ('queued', 'leased');

-- The claim query's access path: ready work in priority order.
CREATE INDEX idx_jobs_claimable ON jobs (state, run_after, priority);

-- Reclaiming expired leases scans by state and expiry.
CREATE INDEX idx_jobs_leases ON jobs (state, lease_expires_at);

-- +goose Down
DROP TABLE jobs;
