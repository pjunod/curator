-- name: InsertMediaItem :one
INSERT INTO media_items (
    kind, title, sort_title, year, author,
    tmdb_id, imdb_id, tvdb_id, isbn13, olid, asin,
    overview, poster_path, backdrop_path, genres, status, release_date, runtime,
    rating, rating_votes, ratings,
    monitored, quality_profile_id, root_folder_id, path, ended, added_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateMediaItemMetadata :exec
-- The metadata.refresh path: re-hydrated provider fields only. Library
-- placement (monitored, profile, root folder, path) is never touched.
UPDATE media_items SET
    title = ?, sort_title = ?, year = ?, author = ?,
    imdb_id = ?, tvdb_id = ?, isbn13 = ?, asin = ?,
    overview = ?, poster_path = ?, backdrop_path = ?, genres = ?,
    status = ?, release_date = ?, runtime = ?,
    rating = ?, rating_votes = ?, ratings = ?, ended = ?, updated_at = ?
WHERE id = ?;

-- name: GetMediaItem :one
SELECT * FROM media_items WHERE id = ?;

-- name: GetMediaItemByKindTmdb :one
SELECT * FROM media_items WHERE kind = ? AND tmdb_id = ?;

-- name: GetMediaItemByKindOlid :one
SELECT * FROM media_items WHERE kind = ? AND olid = ?;

-- name: ListMediaItems :many
SELECT * FROM media_items ORDER BY sort_title, year;

-- name: ListMediaItemsByKind :many
SELECT * FROM media_items WHERE kind = ? ORDER BY sort_title, year;

-- name: ListMediaItemStats :many
-- Completeness per item for list views: monitored aired episodes vs the
-- subset with a file, plus the raw file count (movies/books use that).
SELECT
    m.id,
    (SELECT COUNT(*) FROM episodes e
      WHERE e.media_item_id = m.id AND e.monitored = 1
        AND e.air_date != '' AND e.air_date <= sqlc.arg(today)) AS aired_episodes,
    (SELECT COUNT(DISTINCT mfe.episode_id)
       FROM media_file_episodes mfe
       JOIN episodes e2 ON e2.id = mfe.episode_id
      WHERE e2.media_item_id = m.id AND e2.monitored = 1
        AND e2.air_date != '' AND e2.air_date <= sqlc.arg(today)) AS have_episodes,
    (SELECT COUNT(*) FROM media_files f WHERE f.media_item_id = m.id) AS files
FROM media_items m;

-- name: DeleteMediaItem :exec
DELETE FROM media_items WHERE id = ?;

-- name: TouchMediaItem :exec
UPDATE media_items SET updated_at = ? WHERE id = ?;

-- name: UpdateMediaItemPlacement :exec
-- Per-item edit: monitoring, profile, and where the item lives. Files on
-- disk are never moved by this.
UPDATE media_items SET
    monitored = ?, quality_profile_id = ?, root_folder_id = ?, path = ?,
    updated_at = ?
WHERE id = ?;

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

-- name: UpsertSeasonKeepFlags :exec
-- Refresh path: new seasons appear, existing ones keep their monitored flag.
INSERT INTO seasons (media_item_id, number, monitored)
VALUES (?, ?, ?)
ON CONFLICT (media_item_id, number) DO NOTHING;

-- name: SetSeasonMonitored :execrows
UPDATE seasons SET monitored = ? WHERE media_item_id = ? AND number = ?;

-- name: SetSeasonEpisodesMonitored :exec
-- Season toggles cascade: an unmonitored season wants none of its episodes.
UPDATE episodes SET monitored = ? WHERE media_item_id = ? AND season_number = ?;

-- name: SetEpisodeMonitored :execrows
UPDATE episodes SET monitored = ? WHERE id = ? AND media_item_id = ?;

-- name: UpsertEpisodeMeta :exec
-- Refresh path: provider metadata updates in place; the user's monitored
-- flag survives, and rows are never deleted (files may point at them).
INSERT INTO episodes (
    media_item_id, season_number, episode_number, absolute_num,
    title, air_date, monitored
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (media_item_id, season_number, episode_number) DO UPDATE SET
    absolute_num = excluded.absolute_num,
    title        = excluded.title,
    air_date     = excluded.air_date;

-- name: ListEpisodes :many
SELECT * FROM episodes WHERE media_item_id = ?
ORDER BY season_number, episode_number;

-- name: GetEpisodeByNumber :one
SELECT * FROM episodes
WHERE media_item_id = ? AND season_number = ? AND episode_number = ?;

-- name: InsertRootFolder :one
INSERT INTO root_folders (path, kind, added_at) VALUES (?, ?, ?) RETURNING id;

-- name: UpdateRootFolderKind :exec
UPDATE root_folders SET kind = ? WHERE id = ?;

-- name: ListRootFolders :many
SELECT * FROM root_folders ORDER BY path;

-- name: GetRootFolder :one
SELECT * FROM root_folders WHERE id = ?;

-- name: DeleteRootFolder :exec
DELETE FROM root_folders WHERE id = ?;

-- name: UpsertMediaFile :one
-- copy_id is NOT in the conflict update on purpose: a rescan of a shared
-- folder must never stomp the attribution an import recorded.
INSERT INTO media_files (media_item_id, copy_id, path, size, added_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (path) DO UPDATE SET
    media_item_id = excluded.media_item_id,
    size          = excluded.size
RETURNING id;

-- name: InsertMediaCopy :one
INSERT INTO media_copies (media_item_id, name, quality_profile_id, root_folder_id, path, monitored, added_at)
VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id;

-- name: ListMediaCopies :many
SELECT * FROM media_copies WHERE media_item_id = ? ORDER BY id;

-- name: ListAllMediaCopies :many
SELECT * FROM media_copies ORDER BY media_item_id, id;

-- name: GetMediaCopy :one
SELECT * FROM media_copies WHERE id = ? AND media_item_id = ?;

-- name: UpdateMediaCopy :execrows
UPDATE media_copies SET name = ?, quality_profile_id = ?, monitored = ?
WHERE id = ? AND media_item_id = ?;

-- name: DeleteMediaCopy :execrows
DELETE FROM media_copies WHERE id = ? AND media_item_id = ?;

-- name: DeleteMediaFilesForCopy :exec
DELETE FROM media_files WHERE copy_id = ?;

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
