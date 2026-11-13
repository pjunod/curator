-- +goose Up
-- A catalogue row is not evidence that files are still present. Successful
-- scope checks bind observed file IDs to the exact title/settings revision and
-- root identity. Failure invalidates the proof without deleting library rows.
CREATE TABLE library_presence_roots (
 root_id INTEGER NOT NULL REFERENCES root_folders(id) ON DELETE CASCADE,
 root_path TEXT NOT NULL,
 identity TEXT NOT NULL,
 generation INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(root_id,root_path)
) STRICT;
CREATE TABLE library_presence_observations (
 media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
 copy_id INTEGER NOT NULL,
 item_revision INTEGER NOT NULL,
 presence_fingerprint TEXT NOT NULL,
 shared_revision INTEGER NOT NULL,
 root_id INTEGER NOT NULL REFERENCES root_folders(id) ON DELETE CASCADE,
 root_path TEXT NOT NULL,
 root_identity TEXT NOT NULL,
 root_generation INTEGER NOT NULL,
 scope_path TEXT NOT NULL,
 checked_at INTEGER NOT NULL,
 valid INTEGER NOT NULL CHECK(valid IN(0,1)),
 present_file_ids TEXT NOT NULL DEFAULT '[]',
 PRIMARY KEY(media_item_id,copy_id)
) STRICT;

-- Only records predating this migration may supply automatic first-pin
-- evidence. A scan on an untrusted volume cannot bootstrap itself later.
CREATE TABLE library_presence_bootstrap_files (
 file_id INTEGER PRIMARY KEY,
 media_item_id INTEGER NOT NULL,
 copy_id INTEGER NOT NULL,
 path TEXT NOT NULL,
 size INTEGER NOT NULL,
 file_generation TEXT NOT NULL
) STRICT;
INSERT INTO library_presence_bootstrap_files
 SELECT id,media_item_id,COALESCE(copy_id,0),path,size,file_generation
 FROM media_files WHERE media_item_id IS NOT NULL;

-- +goose Down
DROP TABLE library_presence_bootstrap_files;
DROP TABLE library_presence_observations;
DROP TABLE library_presence_roots;
