-- +goose Up
-- Two things, both for the nzbd integration (nzbd/docs/INTEGRATION_PLAN.md).
--
-- 1. A 'nzbd' download client type. Monarr can already drive nzbd through
--    its NZBGet-compat shim and that keeps working; the native client is a
--    separate type because it is a different API with different
--    capabilities (add-time params, a history cursor, an event stream),
--    and an operator switching between them should be an explicit choice
--    rather than a silent protocol upgrade.
--
-- 2. downloads.transfer — the id that names ONE transfer end to end
--    (contract §3.1: t-<downloads.id>-<6 hex>). It is set on the download
--    row, sent to nzbd as a job param, and will ride to plurx as a
--    correlation id, so grepping any one application's log for it
--    reconstructs the whole story. Empty for downloads that predate this
--    and for clients that cannot carry a param.
--
-- SQLite cannot alter a CHECK, so download_clients is rebuilt — same
-- shape as migrations 0007 and 0012, and client_id in downloads is a
-- plain integer with no FK, so ids survive the copy.

ALTER TABLE downloads ADD COLUMN transfer TEXT NOT NULL DEFAULT '';

CREATE TABLE download_clients_new (
    id              INTEGER PRIMARY KEY,
    type            TEXT NOT NULL CHECK (type IN ('qbittorrent', 'sabnzbd', 'transmission', 'deluge', 'nzbget', 'nzbd')),
    name            TEXT NOT NULL,
    url             TEXT NOT NULL,
    username        TEXT NOT NULL DEFAULT '',
    password        TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT 'monarr',
    enabled         INTEGER NOT NULL DEFAULT 1,
    path_mappings   TEXT NOT NULL DEFAULT '[]',
    manual_approval INTEGER NOT NULL DEFAULT 0,
    added_at        INTEGER NOT NULL
) STRICT;

INSERT INTO download_clients_new
    (id, type, name, url, username, password, category, enabled, path_mappings, manual_approval, added_at)
SELECT id, type, name, url, username, password, category, enabled, path_mappings, manual_approval, added_at
FROM download_clients;

DROP TABLE download_clients;
ALTER TABLE download_clients_new RENAME TO download_clients;
