-- +goose Up
CREATE TABLE import_placements (
 id TEXT PRIMARY KEY,
 target TEXT NOT NULL,
 state TEXT NOT NULL,
 data TEXT NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX import_placements_pending ON import_placements(state,updated_at);
CREATE TABLE recovery_imports (
 id TEXT PRIMARY KEY,
 state TEXT NOT NULL,
 data TEXT NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE TABLE recovery_file_results (
 import_id TEXT NOT NULL REFERENCES recovery_imports(id),
 file_id TEXT NOT NULL,
 result TEXT NOT NULL,
 PRIMARY KEY(import_id,file_id)
);
CREATE TABLE recovery_receipt_outbox (
 import_id TEXT PRIMARY KEY REFERENCES recovery_imports(id),
 receipt TEXT NOT NULL,
 delivered INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL
);


-- A transaction-local revision catches edits made by library APIs while a
-- recovery file is copying. It is intentionally conservative across titles.
CREATE TABLE lifecycle_library_revision (id INTEGER PRIMARY KEY CHECK(id=1), revision INTEGER NOT NULL);
INSERT INTO lifecycle_library_revision VALUES(1,0);
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_items_insert AFTER INSERT ON media_items
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_items_update AFTER UPDATE ON media_items
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_items_delete AFTER DELETE ON media_items
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_copies_insert AFTER INSERT ON media_copies
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_copies_update AFTER UPDATE ON media_copies
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_copies_delete AFTER DELETE ON media_copies
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_quality_profiles_insert AFTER INSERT ON quality_profiles
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_quality_profiles_update AFTER UPDATE ON quality_profiles
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_quality_profiles_delete AFTER DELETE ON quality_profiles
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_seasons_insert AFTER INSERT ON seasons
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_seasons_update AFTER UPDATE ON seasons
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_seasons_delete AFTER DELETE ON seasons
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_episodes_insert AFTER INSERT ON episodes
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_episodes_update AFTER UPDATE ON episodes
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_episodes_delete AFTER DELETE ON episodes
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_files_insert AFTER INSERT ON media_files
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_files_update AFTER UPDATE ON media_files
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_files_delete AFTER DELETE ON media_files
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_file_episodes_insert AFTER INSERT ON media_file_episodes
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_file_episodes_update AFTER UPDATE ON media_file_episodes
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_media_file_episodes_delete AFTER DELETE ON media_file_episodes
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_root_folders_insert AFTER INSERT ON root_folders
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_root_folders_update AFTER UPDATE ON root_folders
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER lifecycle_root_folders_delete AFTER DELETE ON root_folders
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd

CREATE TABLE recovery_previews (
 id TEXT PRIMARY KEY,
 state TEXT NOT NULL,
 request TEXT NOT NULL,
 result TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL
);
CREATE INDEX recovery_previews_pending ON recovery_previews(state,updated_at);

-- +goose Down
DROP TABLE recovery_previews;
DROP TRIGGER lifecycle_media_items_insert;
DROP TRIGGER lifecycle_media_items_update;
DROP TRIGGER lifecycle_media_items_delete;
DROP TRIGGER lifecycle_media_copies_insert;
DROP TRIGGER lifecycle_media_copies_update;
DROP TRIGGER lifecycle_media_copies_delete;
DROP TRIGGER lifecycle_quality_profiles_insert;
DROP TRIGGER lifecycle_quality_profiles_update;
DROP TRIGGER lifecycle_quality_profiles_delete;
DROP TRIGGER lifecycle_seasons_insert;
DROP TRIGGER lifecycle_seasons_update;
DROP TRIGGER lifecycle_seasons_delete;
DROP TRIGGER lifecycle_episodes_insert;
DROP TRIGGER lifecycle_episodes_update;
DROP TRIGGER lifecycle_episodes_delete;
DROP TRIGGER lifecycle_media_files_insert;
DROP TRIGGER lifecycle_media_files_update;
DROP TRIGGER lifecycle_media_files_delete;
DROP TRIGGER lifecycle_media_file_episodes_insert;
DROP TRIGGER lifecycle_media_file_episodes_update;
DROP TRIGGER lifecycle_media_file_episodes_delete;
DROP TRIGGER lifecycle_root_folders_insert;
DROP TRIGGER lifecycle_root_folders_update;
DROP TRIGGER lifecycle_root_folders_delete;
DROP TABLE lifecycle_library_revision;
DROP TABLE recovery_receipt_outbox;
DROP TABLE recovery_file_results;
DROP TABLE recovery_imports;
DROP TABLE import_placements;
