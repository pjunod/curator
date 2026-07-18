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
