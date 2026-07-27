-- name: InsertBlocklist :one
INSERT INTO blocklist (media_item_id, release_title, indexer, reason, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (release_title, indexer) DO UPDATE SET reason = excluded.reason
RETURNING id;

-- name: CountBlocklisted :one
SELECT COUNT(*) FROM blocklist WHERE release_title = ? AND indexer = ?;

-- name: ListBlocklist :many
SELECT * FROM blocklist ORDER BY created_at DESC LIMIT 500;

-- name: DeleteBlocklist :exec
DELETE FROM blocklist WHERE id = ?;

-- name: InsertNotifier :one
INSERT INTO notifiers (type, name, settings, on_grab, on_import, on_failed, on_health, enabled)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: ListNotifiers :many
SELECT * FROM notifiers ORDER BY name;

-- name: GetNotifier :one
SELECT * FROM notifiers WHERE id = ?;

-- name: DeleteNotifier :exec
DELETE FROM notifiers WHERE id = ?;

-- name: ListEpisodesAiring :many
SELECT e.id, e.media_item_id, e.season_number, e.episode_number,
       e.title AS episode_title, e.air_date, m.title AS series_title,
       EXISTS(SELECT 1 FROM media_file_episodes mfe WHERE mfe.episode_id = e.id) AS has_file
FROM episodes e JOIN media_items m ON m.id = e.media_item_id
WHERE e.air_date >= ? AND e.air_date <= ?
ORDER BY e.air_date, m.title, e.season_number, e.episode_number;

-- name: ListItemsReleasedBetween :many
SELECT m.id, m.kind, m.title, m.author, m.release_date,
       EXISTS(SELECT 1 FROM media_files f WHERE f.media_item_id = m.id) AS has_file
FROM media_items m
WHERE m.kind != 'series' AND m.release_date >= ? AND m.release_date <= ?
ORDER BY m.release_date, m.title;

-- name: InsertCustomFormat :one
INSERT INTO custom_formats (name, pattern, score) VALUES (?, ?, ?) RETURNING id;

-- name: ListCustomFormats :many
SELECT * FROM custom_formats ORDER BY name;

-- name: DeleteCustomFormat :exec
DELETE FROM custom_formats WHERE id = ?;

-- name: InsertImportList :one
INSERT INTO import_lists (name, type, config, kind, root_folder_id, quality_profile_id, monitored, enabled)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: ListImportLists :many
SELECT * FROM import_lists ORDER BY name;

-- name: DeleteImportList :exec
DELETE FROM import_lists WHERE id = ?;

-- name: UpdateMediaItemBulk :exec
UPDATE media_items SET
    monitored          = COALESCE(sqlc.narg('monitored'), monitored),
    quality_profile_id = COALESCE(sqlc.narg('quality_profile_id'), quality_profile_id),
    updated_at         = sqlc.arg('updated_at')
WHERE id = sqlc.arg('id');

-- name: UpdateNotifier :exec
UPDATE notifiers
   SET type = ?, name = ?, settings = ?, on_grab = ?, on_import = ?,
       on_failed = ?, on_health = ?, enabled = ?
 WHERE id = ?;

-- name: EnqueueDelivery :one
INSERT INTO notifier_deliveries
    (notifier_id, download_id, event, payload, next_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: DueDeliveries :many
SELECT * FROM notifier_deliveries
 WHERE status = 'pending' AND next_at <= ?
 ORDER BY next_at, id
 LIMIT ?;

-- name: SettleDelivery :exec
UPDATE notifier_deliveries
   SET attempts = ?, last_error = ?, result = ?, status = ?, next_at = ?,
       updated_at = ?
 WHERE id = ?;

-- name: ListDeliveries :many
SELECT * FROM notifier_deliveries
 WHERE notifier_id = ?
 ORDER BY id DESC
 LIMIT ?;

-- name: DeleteDeliveriesForNotifier :exec
DELETE FROM notifier_deliveries WHERE notifier_id = ?;
