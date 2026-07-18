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
