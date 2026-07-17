-- name: ListProfiles :many
SELECT * FROM quality_profiles ORDER BY id;

-- name: GetProfile :one
SELECT * FROM quality_profiles WHERE id = ?;

-- name: InsertIndexer :one
INSERT INTO indexers (name, url, api_key, protocol, categories, enabled, added_at)
VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ListIndexers :many
SELECT * FROM indexers ORDER BY name;

-- name: GetIndexer :one
SELECT * FROM indexers WHERE id = ?;

-- name: DeleteIndexer :exec
DELETE FROM indexers WHERE id = ?;

-- name: InsertDownloadClient :one
INSERT INTO download_clients (type, name, url, username, password, category, enabled, added_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ListDownloadClients :many
SELECT * FROM download_clients ORDER BY name;

-- name: GetDownloadClient :one
SELECT * FROM download_clients WHERE id = ?;

-- name: DeleteDownloadClient :exec
DELETE FROM download_clients WHERE id = ?;

-- name: InsertDownload :one
INSERT INTO downloads (
    media_item_id, wantables, season, release_title, indexer, protocol,
    quality, size, client_id, handle, state, added_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ListActiveDownloads :many
SELECT * FROM downloads
WHERE state IN ('grabbed', 'downloading', 'completed', 'importing')
ORDER BY added_at DESC;

-- name: ListRecentDownloads :many
SELECT * FROM downloads ORDER BY added_at DESC LIMIT 100;

-- name: GetDownload :one
SELECT * FROM downloads WHERE id = ?;

-- name: UpdateDownloadState :exec
UPDATE downloads SET state = ?, progress = ?, error = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteDownload :exec
DELETE FROM downloads WHERE id = ?;

-- name: SetFileQuality :exec
UPDATE media_files SET quality = ? WHERE id = ?;

-- name: ListFileQualitiesForItem :many
SELECT id, quality FROM media_files WHERE media_item_id = ?;

-- name: InsertHistory :exec
INSERT INTO history_events (ts, type, media_item_id, release_title, data)
VALUES (?, ?, ?, ?, ?);

-- name: ListHistory :many
SELECT * FROM history_events ORDER BY ts DESC LIMIT 200;
