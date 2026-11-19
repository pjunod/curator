-- +goose Up
CREATE TABLE activity_selections (
 id TEXT PRIMARY KEY, targets TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE activity_operations (
 id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, action TEXT NOT NULL,
 targets TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE activity_operation_targets (
 operation_id TEXT NOT NULL REFERENCES activity_operations(id), download_id INTEGER NOT NULL,
 generation_key TEXT NOT NULL, state TEXT NOT NULL, payload TEXT NOT NULL,
 PRIMARY KEY(operation_id,download_id)
);
-- A bulk member also owns unacknowledged descendants. Late publications
-- replay that original member without changing the user's frozen selection.
CREATE TABLE activity_disposition_links (
 download_id INTEGER NOT NULL,generation_key TEXT NOT NULL,
 operation_id TEXT NOT NULL REFERENCES activity_operations(id),
 target_id INTEGER NOT NULL,target_generation TEXT NOT NULL,
 PRIMARY KEY(download_id,generation_key,operation_id,target_id)
);
CREATE TABLE activity_action_events (
 id INTEGER PRIMARY KEY, operation_id TEXT NOT NULL, download_id INTEGER NOT NULL,
 generation_key TEXT NOT NULL, at INTEGER NOT NULL, step TEXT NOT NULL, message TEXT NOT NULL
);
CREATE INDEX activity_events_attempt ON activity_action_events(download_id,generation_key,at,id);
CREATE TABLE replacement_admissions (
 parent_id INTEGER NOT NULL,parent_generation TEXT NOT NULL,child_id INTEGER NOT NULL,
 child_generation TEXT NOT NULL,phase TEXT NOT NULL DEFAULT 'admitting',PRIMARY KEY(child_id,child_generation)
);
-- Kept independently of visible history: a late Runner fact must never restart
-- an attempt the operator deleted. This is intent, not a fabricated terminal.
CREATE TABLE download_dispositions (
 download_id INTEGER NOT NULL, generation_key TEXT NOT NULL,
 requested_at INTEGER NOT NULL, verified_at INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'requested', message TEXT NOT NULL DEFAULT '', owner_payload TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 1,
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
 UPDATE download_dispositions SET revision=revision+1,verified_at=0,state='requested',message='Finishing deletion of a late publication' WHERE download_id=NEW.download_id AND generation_key=(SELECT generation_key FROM downloads WHERE id=NEW.download_id);
 UPDATE download_dispositions SET revision=revision+1,verified_at=0,state='requested',message='Finishing deletion of a replacement publication'
 WHERE (download_id,generation_key) IN (SELECT target_id,target_generation FROM activity_disposition_links WHERE download_id=NEW.download_id AND generation_key=(SELECT generation_key FROM downloads WHERE id=NEW.download_id) AND target_id!=NEW.download_id);
 UPDATE activity_operation_targets SET state='pending',payload=json_set(payload,'$.state','pending','$.message','Cleaning a late publication')
 WHERE (operation_id,download_id,generation_key) IN (SELECT l.operation_id,l.target_id,l.target_generation FROM activity_disposition_links l WHERE l.download_id=NEW.download_id AND l.generation_key=(SELECT generation_key FROM downloads WHERE id=NEW.download_id) AND l.operation_id=(SELECT l2.operation_id FROM activity_disposition_links l2 JOIN activity_operations o2 ON o2.id=l2.operation_id WHERE l2.download_id=l.download_id AND l2.generation_key=l.generation_key AND l2.target_id=l.target_id AND l2.target_generation=l.target_generation ORDER BY o2.created_at DESC,o2.id DESC LIMIT 1));
 UPDATE activity_operations SET updated_at=updated_at+1 WHERE id IN (SELECT l.operation_id FROM activity_disposition_links l WHERE l.download_id=NEW.download_id AND l.generation_key=(SELECT generation_key FROM downloads WHERE id=NEW.download_id) AND EXISTS(SELECT 1 FROM activity_operation_targets t WHERE t.operation_id=l.operation_id AND t.download_id=l.target_id AND t.state='pending'));
 INSERT OR IGNORE INTO jobs(kind,payload,priority,run_after,max_attempts,dedupe_key,created_at,updated_at)
 SELECT 'activity.action',json_object('id',o.id),10,0,1000000,'activity:'||o.id,o.updated_at,o.updated_at FROM activity_operations o WHERE EXISTS(SELECT 1 FROM activity_disposition_links l JOIN activity_operation_targets t ON t.operation_id=l.operation_id AND t.download_id=l.target_id WHERE l.download_id=NEW.download_id AND l.generation_key=(SELECT generation_key FROM downloads WHERE id=NEW.download_id) AND l.operation_id=o.id AND t.state='pending');
END;
-- +goose StatementEnd
-- Keep unresolved action authority even when ordinary history expires.
-- +goose StatementBegin
CREATE TRIGGER preserve_pending_disposition BEFORE DELETE ON downloads
WHEN EXISTS(SELECT 1 FROM download_dispositions x WHERE x.download_id=OLD.id AND x.generation_key=OLD.generation_key)
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
 WHERE EXISTS(SELECT 1 FROM activity_operation_targets t WHERE t.operation_id=json_extract(NEW.payload,'$.id') AND t.state IN ('pending','running'));
END;
-- +goose StatementEnd
-- An unacknowledged replacement is still an obligation of its originating
-- attempt. A fully admitted replacement has an independent identity.
-- +goose StatementBegin
CREATE TRIGGER enroll_replacement_dispositions AFTER INSERT ON download_dispositions
BEGIN
 INSERT OR IGNORE INTO download_dispositions(download_id,generation_key,requested_at)
 SELECT child_id,child_generation,NEW.requested_at FROM (
 WITH RECURSIVE obligations(child_id,child_generation) AS (
 SELECT child_id,child_generation FROM replacement_admissions WHERE parent_id=NEW.download_id AND parent_generation=NEW.generation_key AND phase IN ('admitting','cancel_pending')
 UNION
 SELECT r.child_id,r.child_generation FROM replacement_admissions r JOIN obligations o ON r.parent_id=o.child_id AND r.parent_generation=o.child_generation WHERE r.phase IN ('admitting','cancel_pending')
 ) SELECT child_id,child_generation FROM obligations);
 UPDATE replacement_admissions SET phase='cancel_pending' WHERE phase='admitting' AND EXISTS(SELECT 1 FROM download_dispositions d WHERE d.download_id=child_id AND d.generation_key=child_generation);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER record_replacement_admission AFTER UPDATE OF submission_phase ON downloads
WHEN NEW.submission_phase='submitted'
BEGIN
 UPDATE replacement_admissions SET phase=CASE WHEN EXISTS(SELECT 1 FROM download_dispositions x WHERE x.download_id=parent_id AND x.generation_key=parent_generation) THEN 'cancel_pending' ELSE 'admitted' END WHERE child_id=NEW.id AND child_generation=NEW.generation_key AND phase='admitting';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER record_activity_notification AFTER UPDATE ON notifier_deliveries
WHEN NEW.download_id>0 AND (NEW.status!=OLD.status OR NEW.last_error!=OLD.last_error OR NEW.attempts!=OLD.attempts)
BEGIN
 INSERT INTO activity_action_events(operation_id,download_id,generation_key,at,step,message)
 SELECT 'cinema:'||NEW.id,NEW.download_id,d.generation_key,NEW.updated_at,'Cinema update',NEW.status||CASE WHEN NEW.last_error!='' THEN ': '||NEW.last_error ELSE '' END FROM downloads d WHERE d.id=NEW.download_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER record_activity_runner_control AFTER UPDATE OF runner_control ON downloads
WHEN NEW.runner_control!=OLD.runner_control AND json_valid(NEW.runner_control)
BEGIN
 INSERT INTO activity_action_events(operation_id,download_id,generation_key,at,step,message)
 VALUES('runner-control',NEW.id,NEW.generation_key,NEW.updated_at,'Runner control',COALESCE(json_extract(NEW.runner_control,'$.message'),'Runner control changed'));
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER record_activity_runner_control;
DROP TRIGGER record_activity_notification;
DROP TRIGGER record_replacement_admission;
DROP TRIGGER enroll_replacement_dispositions;
DROP TRIGGER replay_pending_activity;
DROP TRIGGER preserve_pending_disposition;
DROP TRIGGER dispose_late_publication;
DROP TRIGGER fence_deleted_publication;
DROP TRIGGER suppress_deleted_replacement;
DROP TABLE replacement_admissions;
DROP TABLE activity_disposition_links;
DROP TABLE activity_action_events;
DROP TABLE activity_operation_targets;
DROP TABLE download_dispositions;
DROP TABLE activity_operations;
DROP TABLE activity_selections;
