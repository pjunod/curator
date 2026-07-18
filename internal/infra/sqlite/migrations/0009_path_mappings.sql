-- +goose Up
-- Remote path mappings per download client (Sonarr's feature of the same
-- name): when the client runs on another host (or another container), the
-- completed-download path it reports is not the path Monarr sees the same
-- files at. JSON array of {"remote": "...", "local": "..."} prefix rules.

ALTER TABLE download_clients ADD COLUMN path_mappings TEXT NOT NULL DEFAULT '[]';
