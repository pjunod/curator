-- name: InsertMediaItem :one
INSERT INTO media_items (
    kind, title, sort_title, year,
    tmdb_id, imdb_id, tvdb_id, isbn13, olid, asin,
    overview, poster_path, backdrop_path, genres, status, release_date, runtime,
    monitored, root_folder_id, path, ended, added_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetMediaItem :one
SELECT * FROM media_items WHERE id = ?;

-- name: GetMediaItemByKindTmdb :one
SELECT * FROM media_items WHERE kind = ? AND tmdb_id = ?;

-- name: ListMediaItems :many
SELECT * FROM media_items ORDER BY sort_title, year;

-- name: ListMediaItemsByKind :many
SELECT * FROM media_items WHERE kind = ? ORDER BY sort_title, year;

-- name: DeleteMediaItem :exec
DELETE FROM media_items WHERE id = ?;

-- name: TouchMediaItem :exec
UPDATE media_items SET updated_at = ? WHERE id = ?;

-- name: InsertSeason :one
INSERT INTO seasons (media_item_id, number, monitored)
VALUES (?, ?, ?)
RETURNING id;

-- name: ListSeasons :many
SELECT * FROM seasons WHERE media_item_id = ? ORDER BY number;

-- name: InsertEpisode :one
INSERT INTO episodes (
    media_item_id, season_number, episode_number, absolute_num,
    title, air_date, monitored
) VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: ListEpisodes :many
SELECT * FROM episodes WHERE media_item_id = ?
ORDER BY season_number, episode_number;

-- name: GetEpisodeByNumber :one
SELECT * FROM episodes
WHERE media_item_id = ? AND season_number = ? AND episode_number = ?;

-- name: InsertRootFolder :one
INSERT INTO root_folders (path, added_at) VALUES (?, ?) RETURNING id;

-- name: ListRootFolders :many
SELECT * FROM root_folders ORDER BY path;

-- name: GetRootFolder :one
SELECT * FROM root_folders WHERE id = ?;

-- name: DeleteRootFolder :exec
DELETE FROM root_folders WHERE id = ?;

-- name: UpsertMediaFile :one
INSERT INTO media_files (media_item_id, path, size, added_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (path) DO UPDATE SET
    media_item_id = excluded.media_item_id,
    size          = excluded.size
RETURNING id;

-- name: ListMediaFilesForItem :many
SELECT * FROM media_files WHERE media_item_id = ? ORDER BY path;

-- name: ListAllMediaFiles :many
SELECT * FROM media_files ORDER BY path;

-- name: DeleteMediaFile :exec
DELETE FROM media_files WHERE id = ?;

-- name: LinkFileEpisode :exec
INSERT INTO media_file_episodes (media_file_id, episode_id)
VALUES (?, ?)
ON CONFLICT DO NOTHING;

-- name: ClearFileEpisodeLinks :exec
DELETE FROM media_file_episodes WHERE media_file_id = ?;

-- name: ListFileEpisodeLinksForItem :many
SELECT mfe.media_file_id, mfe.episode_id
FROM media_file_episodes mfe
JOIN media_files mf ON mf.id = mfe.media_file_id
WHERE mf.media_item_id = ?;
