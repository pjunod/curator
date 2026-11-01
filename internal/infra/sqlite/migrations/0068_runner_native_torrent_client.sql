-- +goose Up
-- Explicit native Runner torrent mode; existing Usenet and ordinary torrent
-- types retain their protocol, configuration and custody behavior. No foreign
-- key references download_clients; preserve IDs, all columns and plan triggers.
CREATE TABLE download_clients_new (
 id INTEGER PRIMARY KEY,
 type TEXT NOT NULL CHECK (type IN ('qbittorrent','sabnzbd','transmission','deluge','nzbget','nzbd','runner-torrent')),
 name TEXT NOT NULL, url TEXT NOT NULL,
 username TEXT NOT NULL DEFAULT '', password TEXT NOT NULL DEFAULT '',
 category TEXT NOT NULL DEFAULT 'monarr', enabled INTEGER NOT NULL DEFAULT 1,
 path_mappings TEXT NOT NULL DEFAULT '[]', manual_approval INTEGER NOT NULL DEFAULT 0,
 added_at INTEGER NOT NULL,
 mode TEXT NOT NULL DEFAULT 'poll' CHECK (mode IN ('poll','push')),
 remove_completed INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO download_clients_new (id,type,name,url,username,password,category,enabled,path_mappings,manual_approval,added_at,mode,remove_completed) SELECT id,type,name,url,username,password,category,enabled,path_mappings,manual_approval,added_at,mode,remove_completed FROM download_clients;
DROP TABLE download_clients;
ALTER TABLE download_clients_new RENAME TO download_clients;
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_insert AFTER INSERT ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_update AFTER UPDATE ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_delete AFTER DELETE ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd

-- Exact bounded dual-mode discovery lookup; unrelated/ambiguous rows remain
-- visible for review rather than relying on title or configured URL equality.
CREATE INDEX downloads_runner_mode_admission ON downloads(transfer,handle,runner_instance_id)
WHERE runner_admission_generation!='';

-- +goose Down
-- Refuse to erase a configured native acquisition's client contract. Older
-- binaries would otherwise construct the wrong adapter for this type.
CREATE TEMP TABLE native_torrent_downgrade_guard (
 ok INTEGER CONSTRAINT native_torrent_clients_require_current_binary CHECK (ok=1)
);
INSERT INTO native_torrent_downgrade_guard SELECT CASE WHEN EXISTS (
 SELECT 1 FROM download_clients WHERE type='runner-torrent'
) THEN 0 ELSE 1 END;
DROP TABLE native_torrent_downgrade_guard;
DROP INDEX downloads_runner_mode_admission;
CREATE TABLE download_clients_new (
 id INTEGER PRIMARY KEY,
 type TEXT NOT NULL CHECK (type IN ('qbittorrent','sabnzbd','transmission','deluge','nzbget','nzbd')),
 name TEXT NOT NULL, url TEXT NOT NULL,
 username TEXT NOT NULL DEFAULT '', password TEXT NOT NULL DEFAULT '',
 category TEXT NOT NULL DEFAULT 'monarr', enabled INTEGER NOT NULL DEFAULT 1,
 path_mappings TEXT NOT NULL DEFAULT '[]', manual_approval INTEGER NOT NULL DEFAULT 0,
 added_at INTEGER NOT NULL,
 mode TEXT NOT NULL DEFAULT 'poll' CHECK (mode IN ('poll','push')),
 remove_completed INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO download_clients_new (id,type,name,url,username,password,category,enabled,path_mappings,manual_approval,added_at,mode,remove_completed) SELECT id,type,name,url,username,password,category,enabled,path_mappings,manual_approval,added_at,mode,remove_completed FROM download_clients;
DROP TABLE download_clients;
ALTER TABLE download_clients_new RENAME TO download_clients;
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_insert AFTER INSERT ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_update AFTER UPDATE ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER acquisition_download_clients_delete AFTER DELETE ON download_clients
BEGIN UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1; END;
-- +goose StatementEnd
