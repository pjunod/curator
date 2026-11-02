-- +goose Up
-- A recovery import is leased against the state it previewed. That lease was
-- lifecycle_library_revision, which every title's edit advances, so a
-- recovery could only run while the whole library was quiet: it excluded
-- every other title's import, and any other title's commit voided it.
--
-- The lease is now scoped to what a recovery actually depends on: its own
-- title (the item, its copies, seasons, episodes, files and their episode
-- links) plus the settings every placement reads (quality profiles, root
-- folders, custom formats). lifecycle_library_revision keeps advancing as
-- before for acquisition plans, which fence library-wide policy.
--
-- An item's row is created by its first change; a missing row reads as 0.
-- Rows are never deleted, so a revision never repeats for an id.
CREATE TABLE lifecycle_item_revision (
 media_item_id INTEGER PRIMARY KEY,
 revision INTEGER NOT NULL
) STRICT;
CREATE TABLE lifecycle_shared_revision (id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL) STRICT;
INSERT INTO lifecycle_shared_revision VALUES(1,0);

-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_items_insert AFTER INSERT ON media_items
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(NEW.id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_items_update AFTER UPDATE ON media_items
BEGIN
 INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT NEW.id,1 WHERE NEW.id IS NOT OLD.id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_items_delete AFTER DELETE ON media_items
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_copies_insert AFTER INSERT ON media_copies
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(NEW.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_copies_update AFTER UPDATE ON media_copies
BEGIN
 INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT NEW.media_item_id,1 WHERE NEW.media_item_id IS NOT OLD.media_item_id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_copies_delete AFTER DELETE ON media_copies
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_seasons_insert AFTER INSERT ON seasons
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(NEW.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_seasons_update AFTER UPDATE ON seasons
BEGIN
 INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT NEW.media_item_id,1 WHERE NEW.media_item_id IS NOT OLD.media_item_id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_seasons_delete AFTER DELETE ON seasons
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_episodes_insert AFTER INSERT ON episodes
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(NEW.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_episodes_update AFTER UPDATE ON episodes
BEGIN
 INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT NEW.media_item_id,1 WHERE NEW.media_item_id IS NOT OLD.media_item_id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_episodes_delete AFTER DELETE ON episodes
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) VALUES(OLD.media_item_id,1) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd

-- media_files.media_item_id is nullable: an unowned file advances no title.
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_files_insert AFTER INSERT ON media_files
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT NEW.media_item_id,1 WHERE NEW.media_item_id IS NOT NULL ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_files_update AFTER UPDATE ON media_files
BEGIN
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT OLD.media_item_id,1 WHERE OLD.media_item_id IS NOT NULL ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT NEW.media_item_id,1 WHERE NEW.media_item_id IS NOT NULL AND NEW.media_item_id IS NOT OLD.media_item_id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_files_delete AFTER DELETE ON media_files
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT OLD.media_item_id,1 WHERE OLD.media_item_id IS NOT NULL ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd

-- An episode link belongs to the title of its episode.
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_file_episodes_insert AFTER INSERT ON media_file_episodes
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT media_item_id,1 FROM episodes WHERE id=NEW.episode_id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_file_episodes_update AFTER UPDATE ON media_file_episodes
BEGIN
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT media_item_id,1 FROM episodes WHERE id=OLD.episode_id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
 INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT e.media_item_id,1 FROM episodes e WHERE e.id=NEW.episode_id AND e.media_item_id IS NOT (SELECT media_item_id FROM episodes WHERE id=OLD.episode_id) ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_item_media_file_episodes_delete AFTER DELETE ON media_file_episodes
BEGIN INSERT INTO lifecycle_item_revision(media_item_id,revision) SELECT media_item_id,1 FROM episodes WHERE id=OLD.episode_id ON CONFLICT(media_item_id) DO UPDATE SET revision=revision+1; END;
-- +goose StatementEnd

-- Settings every placement reads advance every title's lease.
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_quality_profiles_insert AFTER INSERT ON quality_profiles
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_quality_profiles_update AFTER UPDATE ON quality_profiles
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_quality_profiles_delete AFTER DELETE ON quality_profiles
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_root_folders_insert AFTER INSERT ON root_folders
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_root_folders_update AFTER UPDATE ON root_folders
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_root_folders_delete AFTER DELETE ON root_folders
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_custom_formats_insert AFTER INSERT ON custom_formats
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_custom_formats_update AFTER UPDATE ON custom_formats
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_shared_custom_formats_delete AFTER DELETE ON custom_formats
BEGIN UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER lifecycle_shared_custom_formats_delete;
DROP TRIGGER lifecycle_shared_custom_formats_update;
DROP TRIGGER lifecycle_shared_custom_formats_insert;
DROP TRIGGER lifecycle_shared_root_folders_delete;
DROP TRIGGER lifecycle_shared_root_folders_update;
DROP TRIGGER lifecycle_shared_root_folders_insert;
DROP TRIGGER lifecycle_shared_quality_profiles_delete;
DROP TRIGGER lifecycle_shared_quality_profiles_update;
DROP TRIGGER lifecycle_shared_quality_profiles_insert;
DROP TRIGGER lifecycle_item_media_file_episodes_delete;
DROP TRIGGER lifecycle_item_media_file_episodes_update;
DROP TRIGGER lifecycle_item_media_file_episodes_insert;
DROP TRIGGER lifecycle_item_media_files_delete;
DROP TRIGGER lifecycle_item_media_files_update;
DROP TRIGGER lifecycle_item_media_files_insert;
DROP TRIGGER lifecycle_item_episodes_delete;
DROP TRIGGER lifecycle_item_episodes_update;
DROP TRIGGER lifecycle_item_episodes_insert;
DROP TRIGGER lifecycle_item_seasons_delete;
DROP TRIGGER lifecycle_item_seasons_update;
DROP TRIGGER lifecycle_item_seasons_insert;
DROP TRIGGER lifecycle_item_media_copies_delete;
DROP TRIGGER lifecycle_item_media_copies_update;
DROP TRIGGER lifecycle_item_media_copies_insert;
DROP TRIGGER lifecycle_item_media_items_delete;
DROP TRIGGER lifecycle_item_media_items_update;
DROP TRIGGER lifecycle_item_media_items_insert;
DROP TABLE lifecycle_shared_revision;
DROP TABLE lifecycle_item_revision;
-- An older binary leases recovery against lifecycle_library_revision through
-- $.accepted_revision, which this version does not write. Active recoveries
-- go back to review so they are previewed again rather than run unleased.
UPDATE recovery_imports SET state='review',
 data=json_set(data,'$.error','Recovery needs a new preview after a downgrade')
 WHERE state IN ('queued','importing');
