-- +goose Up
-- ADR 0009 §4, the retrospective half of exclusions: "I looked, it is not
-- media, stop asking". Without this a dismissed candidate is re-offered on
-- every scan forever, which is the actual lived complaint — the prospective
-- half (skip patterns) lives in code as defaults plus a setting, because it
-- is keyed by name rather than by path.

CREATE TABLE ignored_paths (
    path       TEXT PRIMARY KEY,
    reason     TEXT NOT NULL DEFAULT '',
    ignored_at INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE ignored_paths;
