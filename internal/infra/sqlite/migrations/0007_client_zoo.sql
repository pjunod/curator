-- +goose Up
-- The download_clients CHECK predates the Phase 5 client zoo
-- (transmission/deluge/nzbget) and rejected the new types at insert.
-- SQLite can't alter a CHECK, so rebuild the table with the full set.
-- downloads.client_id is a plain integer (no FK), so ids must — and do —
-- survive the copy.

CREATE TABLE download_clients_new (
    id       INTEGER PRIMARY KEY,
    type     TEXT NOT NULL CHECK (type IN ('qbittorrent', 'sabnzbd', 'transmission', 'deluge', 'nzbget')),
    name     TEXT NOT NULL,
    url      TEXT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    password TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL DEFAULT 'monarr',
    enabled  INTEGER NOT NULL DEFAULT 1,
    added_at INTEGER NOT NULL
) STRICT;

INSERT INTO download_clients_new (id, type, name, url, username, password, category, enabled, added_at)
SELECT id, type, name, url, username, password, category, enabled, added_at FROM download_clients;

DROP TABLE download_clients;
ALTER TABLE download_clients_new RENAME TO download_clients;
