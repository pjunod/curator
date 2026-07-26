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
INSERT INTO download_clients (type, name, url, username, password, category, enabled, path_mappings, manual_approval, mode, added_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ListDownloadClients :many
SELECT * FROM download_clients ORDER BY name;

-- name: GetDownloadClient :one
SELECT * FROM download_clients WHERE id = ?;

-- name: UpdateDownloadClient :exec
UPDATE download_clients
SET type = ?, name = ?, url = ?, username = ?, password = ?, category = ?,
    enabled = ?, path_mappings = ?, manual_approval = ?, mode = ?
WHERE id = ?;

-- name: DeleteDownloadClient :exec
DELETE FROM download_clients WHERE id = ?;

-- name: InsertDownload :one
INSERT INTO downloads (
    media_item_id, copy_id, wantables, season, release_title, indexer, protocol,
    quality, size, client_id, handle, state, added_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ListActiveDownloads :many
SELECT * FROM downloads
WHERE state IN ('grabbed', 'downloading', 'downloaded', 'awaiting_import', 'importing')
ORDER BY added_at DESC;

-- name: ListInFlightDownloads :many
SELECT * FROM downloads WHERE state != 'imported' ORDER BY added_at DESC;

-- name: ListRecentDownloads :many
SELECT * FROM downloads ORDER BY added_at DESC LIMIT 100;

-- name: GetDownload :one
SELECT * FROM downloads WHERE id = ?;

-- name: UpdateDownloadState :exec
UPDATE downloads SET state = ?, progress = ?, error = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateDownloadHandoff :exec
UPDATE downloads
SET state = ?, progress = ?, error = ?, save_path = ?, import_path = ?,
    handoff_log = ?, updated_at = ?
WHERE id = ?;

-- name: SetDownloadHandle :exec
UPDATE downloads SET handle = ?, transfer = ?, updated_at = ? WHERE id = ?;

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

-- name: SetFileMediaInfo :exec
-- The measured record and where the recorded quality came from (ADR 0013 section 3).
-- Written by the probe path only; `quality` is set separately so a probe that
-- learns nothing new about quality still records that it ran.
UPDATE media_files
SET media_info = ?, quality_provenance = ?, quality_confidence = ?, probed_at = ?
WHERE id = ?;

-- name: SetFileQualityWithProvenance :exec
UPDATE media_files
SET quality = ?, quality_provenance = ?, quality_confidence = ?
WHERE id = ?;

-- name: ListFileQualityRecordsForItem :many
SELECT id, copy_id, path, size, quality, media_info, quality_provenance,
       quality_confidence, probed_at
FROM media_files WHERE media_item_id = ?;

-- name: GetMediaFile :one
SELECT id, media_item_id, copy_id, path, size, added_at, quality,
       media_info, quality_provenance, quality_confidence, probed_at
FROM media_files WHERE id = ?;

-- name: InsertProfile :one
INSERT INTO quality_profiles (name, definition, upgrades_allowed)
VALUES (?, ?, ?) RETURNING id;

-- name: UpdateProfile :execrows
UPDATE quality_profiles SET name = ?, definition = ?, upgrades_allowed = ?
WHERE id = ?;

-- name: DeleteProfile :execrows
DELETE FROM quality_profiles WHERE id = ?;

-- name: CountProfileReferences :one
-- A profile in use cannot be deleted: items, copies and import lists all
-- reference it by id, and SQLite would happily leave them pointing at nothing.
SELECT
  (SELECT COUNT(*) FROM media_items  mi WHERE mi.quality_profile_id = ?1) +
  (SELECT COUNT(*) FROM media_copies mc WHERE mc.quality_profile_id = ?1) +
  (SELECT COUNT(*) FROM import_lists il WHERE il.quality_profile_id = ?1) AS refs;
