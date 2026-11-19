-- +goose Up
CREATE TABLE activity_selections (
 id TEXT PRIMARY KEY, targets TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE activity_operations (
 id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, action TEXT NOT NULL,
 targets TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
-- Kept independently of visible history: a late Runner fact must never restart
-- an attempt the operator deleted. This is intent, not a fabricated terminal.
CREATE TABLE download_dispositions (
 download_id INTEGER NOT NULL, generation_key TEXT NOT NULL,
 requested_at INTEGER NOT NULL, verified_at INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'requested', message TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(download_id,generation_key)
);
-- +goose StatementBegin
CREATE TRIGGER suppress_deleted_replacement BEFORE INSERT ON replacement_grabs
WHEN EXISTS(SELECT 1 FROM download_dispositions x JOIN downloads d ON d.id=x.download_id AND d.generation_key=x.generation_key WHERE x.download_id=NEW.download_id)
BEGIN SELECT RAISE(IGNORE); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER fence_deleted_publication BEFORE INSERT ON import_publication_authorizations
WHEN EXISTS(SELECT 1 FROM download_dispositions x WHERE x.download_id=NEW.download_id AND x.generation_key=NEW.generation_key)
BEGIN SELECT RAISE(ABORT,'job deletion requested'); END;
-- +goose StatementEnd
-- New publications can arrive after their transfer terminal. Preserve that
-- fact, revoke completion and replay the same operator intent through jobs.
-- +goose StatementBegin
CREATE TRIGGER dispose_late_publication AFTER INSERT ON publications
WHEN EXISTS(SELECT 1 FROM download_dispositions x JOIN downloads d ON d.id=x.download_id AND d.generation_key=x.generation_key WHERE d.id=NEW.download_id)
BEGIN
 UPDATE download_dispositions SET verified_at=0,state='requested',message='Finishing deletion of a late publication' WHERE download_id=NEW.download_id AND generation_key=(SELECT generation_key FROM downloads WHERE id=NEW.download_id);
 UPDATE activity_operations SET targets=(SELECT json_group_array(json(CASE WHEN json_extract(t.value,'$.id')=NEW.download_id AND json_extract(t.value,'$.generationKey')=(SELECT generation_key FROM downloads WHERE id=NEW.download_id) THEN json_set(t.value,'$.state','pending','$.message','Cleaning a late publication') ELSE t.value END)) FROM json_each(targets) t),updated_at=updated_at+1
 WHERE id=(SELECT o.id FROM activity_operations o WHERE o.action='delete' AND EXISTS(SELECT 1 FROM json_each(o.targets) t WHERE json_extract(t.value,'$.id')=NEW.download_id AND json_extract(t.value,'$.generationKey')=(SELECT generation_key FROM downloads WHERE id=NEW.download_id)) ORDER BY o.created_at DESC LIMIT 1);
 INSERT OR IGNORE INTO jobs(kind,payload,priority,run_after,max_attempts,dedupe_key,created_at,updated_at)
 SELECT 'activity.action',json_object('id',o.id),10,0,1000000,'activity:'||o.id,o.created_at,o.created_at FROM activity_operations o WHERE o.action='delete' AND EXISTS(SELECT 1 FROM json_each(o.targets) t WHERE json_extract(t.value,'$.id')=NEW.download_id AND json_extract(t.value,'$.generationKey')=(SELECT generation_key FROM downloads WHERE id=NEW.download_id)) ORDER BY o.created_at DESC LIMIT 1;
END;
-- +goose StatementEnd
-- Keep unresolved action authority even when ordinary history expires.
-- +goose StatementBegin
CREATE TRIGGER preserve_pending_disposition BEFORE DELETE ON downloads
WHEN EXISTS(SELECT 1 FROM download_dispositions x WHERE x.download_id=OLD.id AND x.generation_key=OLD.generation_key AND x.verified_at=0)
BEGIN SELECT RAISE(IGNORE); END;
-- +goose StatementEnd
-- Close the gap between the worker's final save and lease completion: a
-- late publication that reused a currently leased dedupe key still gets work.
-- +goose StatementBegin
CREATE TRIGGER replay_pending_activity AFTER UPDATE OF state ON jobs
WHEN NEW.kind='activity.action' AND NEW.state IN ('done','failed')
BEGIN
 INSERT OR IGNORE INTO jobs(kind,payload,priority,run_after,max_attempts,dedupe_key,created_at,updated_at)
 SELECT 'activity.action',NEW.payload,10,0,1000000,NEW.dedupe_key,NEW.updated_at,NEW.updated_at
 WHERE EXISTS(SELECT 1 FROM activity_operations o,json_each(o.targets) t WHERE o.id=json_extract(NEW.payload,'$.id') AND json_extract(t.value,'$.state') IN ('pending','running'));
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER replay_pending_activity;
DROP TRIGGER preserve_pending_disposition;
DROP TRIGGER dispose_late_publication;
DROP TRIGGER fence_deleted_publication;
DROP TRIGGER suppress_deleted_replacement;
DROP TABLE download_dispositions;
DROP TABLE activity_operations;
DROP TABLE activity_selections;
