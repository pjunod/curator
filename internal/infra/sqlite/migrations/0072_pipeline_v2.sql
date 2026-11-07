-- +goose NO TRANSACTION
-- +goose Up
-- Pipeline contract v2, M2 (docs/PIPELINE_CONTRACT.md section 3): one reducer
-- drives the download row from Runner's facts; a terminal is recorded once, in
-- SQL, together with the work it implies; publications (one per Runner
-- generation) become the import unit with a per-file outcome each.
--
-- 1. downloads gains the new states held, submitting and notified. The state
--    CHECK lives on the table, so this is the create-new -> copy -> drop ->
--    rename dance again, and the columns the reducer owns are appended at the
--    end (generated queries name their columns, so order is preserved).
--
--    The DSN opens the database with foreign_keys(1). Two live tables
--    reference downloads(id) ON DELETE CASCADE (import_attempt_controls,
--    runner_admission_poll_schedule). With foreign keys on, DROP TABLE
--    downloads performs an implicit DELETE that RUNS THE CASCADES and wipes
--    those rows. PRAGMA foreign_keys is a no-op inside a transaction, so this
--    file runs outside goose's transaction and manages its own.
PRAGMA foreign_keys = OFF;

-- +goose StatementBegin
BEGIN;

CREATE TABLE downloads_v2 (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    copy_id       INTEGER,
    wantables     TEXT NOT NULL DEFAULT '[]',
    season        INTEGER NOT NULL DEFAULT -1,
    release_title TEXT NOT NULL,
    indexer       TEXT NOT NULL DEFAULT '',
    protocol      TEXT NOT NULL,
    quality       TEXT NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    client_id     INTEGER NOT NULL DEFAULT 0,
    handle        TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'grabbed'
        CHECK (state IN ('planned','grabbed','submitting','downloading','held','downloaded','awaiting_import','importing','imported','notified','failed')),
    progress      REAL NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    save_path     TEXT NOT NULL DEFAULT '',
    import_path   TEXT NOT NULL DEFAULT '',
    handoff_log   TEXT NOT NULL DEFAULT '[]',
    added_at      INTEGER NOT NULL,
    transfer TEXT NOT NULL DEFAULT '',
    payload_removed INTEGER NOT NULL DEFAULT 0,
    match_evidence TEXT NOT NULL DEFAULT '{}',
    runner_control TEXT NOT NULL DEFAULT '',
    plan_id INTEGER REFERENCES acquisition_plans(id),
    candidate_key TEXT NOT NULL DEFAULT '',
    submission_phase TEXT NOT NULL DEFAULT 'submitted' CHECK(submission_phase IN ('pending','submitting','submitted','uncertain','rejected')),
    execution_payload TEXT NOT NULL DEFAULT '{}',
    reserved_episodes TEXT NOT NULL DEFAULT '[]',
    parked_reason TEXT NOT NULL DEFAULT '',
    parked_at INTEGER NOT NULL DEFAULT 0,
    superseded INTEGER NOT NULL DEFAULT 0,
    observation TEXT NOT NULL DEFAULT '{}',
    cleanup_pending INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL,
    activity_dismissed INTEGER NOT NULL DEFAULT 0,
    generation_key TEXT NOT NULL DEFAULT '',
    intent TEXT NOT NULL DEFAULT 'automatic' CHECK (intent IN ('automatic','manual_additional')),
    request_id TEXT NOT NULL DEFAULT '',
    request_fingerprint TEXT NOT NULL DEFAULT '',
    blacklist_override INTEGER NOT NULL DEFAULT 0 CHECK (blacklist_override IN (0,1)),
    delivery_expected INTEGER NOT NULL DEFAULT 0 CHECK (delivery_expected IN (0,1)),
    request_token_idempotent INTEGER NOT NULL DEFAULT 0 CHECK (request_token_idempotent IN (0,1)),
    manual_retry_at INTEGER NOT NULL DEFAULT 0,
    manual_retry_attempts INTEGER NOT NULL DEFAULT 0,
    manual_blocked_at INTEGER NOT NULL DEFAULT 0,
    replacement_operation_id TEXT NOT NULL DEFAULT '',
    source_guid TEXT NOT NULL DEFAULT '',
    source_indexer_id INTEGER NOT NULL DEFAULT 0,
    runner_instance_id TEXT NOT NULL DEFAULT '',
    runner_admission_generation TEXT NOT NULL DEFAULT '',
    runner_submission_retry_at INTEGER NOT NULL DEFAULT 0,
    runner_submission_retry_attempts INTEGER NOT NULL DEFAULT 0,
    runner_submission_blocked_at INTEGER NOT NULL DEFAULT 0,
    provider_retry_after INTEGER NOT NULL DEFAULT 0,
    exact_transfer_correlation INTEGER NOT NULL DEFAULT 0 CHECK(exact_transfer_correlation IN (0,1)),
    -- The reducer's columns (contract section 3.2). runner_outcome is the
    -- terminal, set once by RecordRunnerOutcome; provisional marks one Curator
    -- authored (submit, lost, a migrated park) that any later Runner fact may
    -- re-open.
    runner_outcome TEXT NOT NULL DEFAULT '' CHECK (runner_outcome IN ('','completed','failed','cancelled')),
    runner_outcome_at INTEGER NOT NULL DEFAULT 0,
    runner_outcome_seq INTEGER NOT NULL DEFAULT 0,
    runner_outcome_reason TEXT NOT NULL DEFAULT '',
    runner_fetch_cause TEXT NOT NULL DEFAULT '',
    runner_blames_release INTEGER NOT NULL DEFAULT 0 CHECK (runner_blames_release IN (0,1)),
    runner_salvage TEXT NOT NULL DEFAULT '',
    runner_fetched_json TEXT NOT NULL DEFAULT '{}',
    provisional INTEGER NOT NULL DEFAULT 0 CHECK (provisional IN (0,1)),
    deadline_at INTEGER NOT NULL DEFAULT 0,
    state_entered_at INTEGER NOT NULL DEFAULT 0
) STRICT;

INSERT INTO downloads_v2 (
    id, media_item_id, copy_id, wantables, season, release_title, indexer, protocol, quality, size,
    client_id, handle, state, progress, error, save_path, import_path, handoff_log, added_at, transfer,
    payload_removed, match_evidence, runner_control, plan_id, candidate_key, submission_phase,
    execution_payload, reserved_episodes, parked_reason, parked_at, superseded, observation,
    cleanup_pending, updated_at, activity_dismissed, generation_key, intent, request_id,
    request_fingerprint, blacklist_override, delivery_expected, request_token_idempotent,
    manual_retry_at, manual_retry_attempts, manual_blocked_at, replacement_operation_id, source_guid,
    source_indexer_id, runner_instance_id, runner_admission_generation, runner_submission_retry_at,
    runner_submission_retry_attempts, runner_submission_blocked_at, provider_retry_after,
    exact_transfer_correlation, state_entered_at)
SELECT
    id, media_item_id, copy_id, wantables, season, release_title, indexer, protocol, quality, size,
    client_id, handle, state, progress, error, save_path, import_path, handoff_log, added_at, transfer,
    payload_removed, match_evidence, runner_control, plan_id, candidate_key, submission_phase,
    execution_payload, reserved_episodes, parked_reason, parked_at, superseded, observation,
    cleanup_pending, updated_at, activity_dismissed, generation_key, intent, request_id,
    request_fingerprint, blacklist_override, delivery_expected, request_token_idempotent,
    manual_retry_at, manual_retry_attempts, manual_blocked_at, replacement_operation_id, source_guid,
    source_indexer_id, runner_instance_id, runner_admission_generation, runner_submission_retry_at,
    runner_submission_retry_attempts, runner_submission_blocked_at, provider_retry_after,
    exact_transfer_correlation, updated_at
FROM downloads;

DROP TABLE downloads;
ALTER TABLE downloads_v2 RENAME TO downloads;

-- Every index and trigger the old table had, verbatim.
CREATE INDEX download_failure_window ON downloads(state,added_at);
CREATE UNIQUE INDEX download_plan_candidate ON downloads(plan_id,candidate_key) WHERE plan_id IS NOT NULL;
CREATE UNIQUE INDEX downloads_generation_key ON downloads(generation_key);
CREATE UNIQUE INDEX downloads_request_id ON downloads(request_id) WHERE request_id != '';
CREATE INDEX downloads_runner_mode_admission ON downloads(transfer,handle,runner_instance_id) WHERE runner_admission_generation!='';
CREATE INDEX idx_downloads_state ON downloads(state);
-- New: the reducer finds a row by its client and transfer token.
CREATE INDEX downloads_client_transfer ON downloads(client_id,transfer) WHERE transfer != '';

CREATE TRIGGER acquisition_download_terminal_custody AFTER UPDATE OF state ON downloads
WHEN NEW.state IN ('imported','failed') AND NEW.submission_phase NOT IN ('submitting','uncertain') AND NEW.parked_reason NOT LIKE 'operator cancellation%'
BEGIN
 UPDATE downloads SET parked_at=0,parked_reason=CASE WHEN cleanup_pending=0 THEN '' ELSE parked_reason END WHERE id=NEW.id;
END;

CREATE TRIGGER completed_receipt_update AFTER UPDATE ON downloads
WHEN NEW.import_path != ''
BEGIN
    INSERT INTO completed_receipts
        (download_id, added_at, path, client_id, title, state, protocol,
         payload_removed, generation_key)
    VALUES (NEW.id, NEW.added_at, NEW.import_path, NEW.client_id, NEW.release_title,
            NEW.state, NEW.protocol, NEW.payload_removed, NEW.generation_key)
    ON CONFLICT(generation_key, path) DO UPDATE SET
        added_at=excluded.added_at, client_id=excluded.client_id,
        title=excluded.title, state=excluded.state, protocol=excluded.protocol,
        payload_removed=excluded.payload_removed;
END;

CREATE TRIGGER download_generation_immutable BEFORE UPDATE OF generation_key ON downloads
WHEN OLD.generation_key != '' AND NEW.generation_key != OLD.generation_key
BEGIN
    SELECT RAISE(ABORT, 'download generation key is immutable');
END;

CREATE TRIGGER download_generation_insert AFTER INSERT ON downloads
BEGIN
    UPDATE downloads
       SET generation_key = CASE WHEN NEW.generation_key = ''
                                 THEN lower(hex(randomblob(16)))
                                 ELSE NEW.generation_key END
     WHERE id = NEW.id;
    INSERT INTO completed_receipts
        (download_id, added_at, path, client_id, title, state, protocol,
         payload_removed, generation_key)
    SELECT id, added_at, import_path, client_id, release_title, state, protocol,
           payload_removed, generation_key
      FROM downloads WHERE id = NEW.id AND import_path != ''
    ON CONFLICT(generation_key, path) DO UPDATE SET
        added_at=excluded.added_at, client_id=excluded.client_id,
        title=excluded.title, state=excluded.state, protocol=excluded.protocol,
        payload_removed=excluded.payload_removed;
END;

CREATE TRIGGER prevent_cancelled_import_delete BEFORE DELETE ON downloads
WHEN EXISTS(SELECT 1 FROM import_attempt_controls c WHERE c.download_id=OLD.id AND c.outcome IN ('pending','cancelled'))
 OR EXISTS(SELECT 1 FROM import_attempt_stages s WHERE s.download_id=OLD.id AND s.state='held')
 OR EXISTS(SELECT 1 FROM import_placements p WHERE json_extract(p.data,'$.download_id')=OLD.id AND p.state IN ('prepared','retained'))
 OR EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.download_id=OLD.id AND (a.outcome='pending' OR a.resolution='retained_for_review'))
BEGIN
    SELECT RAISE(ABORT,'download retains import cancellation or publication custody');
END;

CREATE TRIGGER prevent_download_delete_with_runner_delivery
BEFORE DELETE ON downloads
WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts r
    WHERE r.download_id=OLD.id AND (r.state!='imported' OR r.ack_state!='accepted'))
BEGIN
    SELECT RAISE(ABORT, 'download has unresolved Runner delivery custody');
END;

-- runner_outcome is written once. Only a provisional (Curator-authored)
-- terminal may be re-opened by a later Runner fact (contract section 3.2).
CREATE TRIGGER downloads_runner_outcome_immutable BEFORE UPDATE OF runner_outcome ON downloads
WHEN OLD.runner_outcome != '' AND OLD.provisional = 0 AND NEW.runner_outcome != OLD.runner_outcome
BEGIN
    SELECT RAISE(ABORT, 'runner_outcome is immutable');
END;

-- deadline_at is written on every state entry (contract section 3.5). A
-- writer that knows better (the reducer: a hold's own deadline) sets it in the
-- same UPDATE; otherwise these triggers give the state its default, and a
-- download whose progress moves gets its allowance again.
CREATE TRIGGER downloads_deadline_on_insert AFTER INSERT ON downloads
WHEN NEW.deadline_at = 0 AND NEW.state IN ('grabbed','submitting','downloading','downloaded','importing')
BEGIN
    UPDATE downloads SET deadline_at = CAST(strftime('%s','now') AS INTEGER) * 1000 + CASE NEW.state
        WHEN 'downloading' THEN CASE WHEN NEW.protocol = 'torrent' THEN 86400000 ELSE 1800000 END
        WHEN 'downloaded' THEN 21600000 WHEN 'importing' THEN 21600000
        ELSE 600000 END,
        state_entered_at = CAST(strftime('%s','now') AS INTEGER) * 1000
     WHERE id = NEW.id;
END;

CREATE TRIGGER downloads_deadline_on_state AFTER UPDATE OF state ON downloads
WHEN NEW.state != OLD.state AND NEW.deadline_at = OLD.deadline_at
BEGIN
    UPDATE downloads SET deadline_at = CASE
        WHEN NEW.state IN ('grabbed','submitting') THEN CAST(strftime('%s','now') AS INTEGER) * 1000 + 600000
        WHEN NEW.state = 'downloading' THEN CAST(strftime('%s','now') AS INTEGER) * 1000 +
             CASE WHEN NEW.protocol = 'torrent' THEN 86400000 ELSE 1800000 END
        WHEN NEW.state = 'held' THEN CAST(strftime('%s','now') AS INTEGER) * 1000 + 86400000
        WHEN NEW.state IN ('downloaded','importing') THEN CAST(strftime('%s','now') AS INTEGER) * 1000 + 21600000
        ELSE 0 END,
        state_entered_at = CAST(strftime('%s','now') AS INTEGER) * 1000
     WHERE id = NEW.id;
END;

CREATE TRIGGER downloads_deadline_on_progress AFTER UPDATE OF progress ON downloads
WHEN NEW.state = 'downloading' AND OLD.state = 'downloading' AND NEW.progress != OLD.progress AND NEW.deadline_at = OLD.deadline_at
BEGIN
    UPDATE downloads SET deadline_at = CAST(strftime('%s','now') AS INTEGER) * 1000 +
        CASE WHEN NEW.protocol = 'torrent' THEN 86400000 ELSE 1800000 END
     WHERE id = NEW.id;
END;

-- 2. publications: the import unit, one per Runner generation, plus the
--    historical directories that exist at cutover (contract section 3.4).
CREATE TABLE publications (
    id INTEGER PRIMARY KEY,
    download_id INTEGER REFERENCES downloads(id) ON DELETE CASCADE,
    generation TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK (kind IN ('completed','salvage')),
    path TEXT NOT NULL UNIQUE,
    remote_path TEXT NOT NULL DEFAULT '',
    manifest_json TEXT NOT NULL DEFAULT '{}',
    manifest_source TEXT NOT NULL DEFAULT 'none' CHECK (manifest_source IN ('file','v1_record','none')),
    cross_volume INTEGER NOT NULL DEFAULT 0 CHECK (cross_volume IN (0,1)),
    state TEXT NOT NULL DEFAULT 'discovered'
        CHECK (state IN ('discovered','importing','imported','import_failed','notified','removed')),
    cohort TEXT NOT NULL DEFAULT 'live' CHECK (cohort IN ('live','historical')),
    cleanup_approved_at INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '',
    discovered_at INTEGER NOT NULL,
    deadline_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX publications_download_generation ON publications (download_id, generation) WHERE download_id IS NOT NULL;
CREATE INDEX publications_state ON publications (state, discovered_at);

CREATE TABLE publication_files (
    publication_id INTEGER NOT NULL REFERENCES publications(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    bytes INTEGER NOT NULL,
    sha256 TEXT NOT NULL DEFAULT '',
    wantable_id TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT 'pending'
        CHECK (outcome IN ('pending','placed','discarded','unresolved')),
    discard_reason TEXT NOT NULL DEFAULT '' CHECK (discard_reason IN ('','superseded','duplicate','rejected')),
    library_path TEXT NOT NULL DEFAULT '',
    placed_sha256 TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    removed_at INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (publication_id, path)
) STRICT;

-- 3. The work a failed terminal implies, written in the terminal's own
--    transaction and executed idempotently by the replacement grab worker.
CREATE TABLE replacement_grabs (
    download_id INTEGER PRIMARY KEY REFERENCES downloads(id) ON DELETE CASCADE,
    wantable_ids TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    attempted_at INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    result TEXT NOT NULL DEFAULT '',
    next_at INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX replacement_grabs_due ON replacement_grabs (result, next_at);

-- 4. Which attempt Curator currently intends for each wantable: the
--    destination-conflict precedence of contract section 3.4. Every download
--    row Curator inserts is a decision for its wantables, so the triggers below
--    record it at the one place every insert path passes through.
CREATE TABLE wantable_attempts (
    wantable_id TEXT PRIMARY KEY,
    download_id INTEGER NOT NULL,
    decided_at INTEGER NOT NULL,
    decided_by TEXT NOT NULL
) STRICT;

CREATE TRIGGER wantable_attempt_on_insert AFTER INSERT ON downloads
BEGIN
    INSERT OR REPLACE INTO wantable_attempts (wantable_id, download_id, decided_at, decided_by)
    SELECT j.value, NEW.id, NEW.added_at,
           CASE WHEN NEW.replacement_operation_id != '' THEN 'replacement' WHEN NEW.plan_id IS NOT NULL THEN 'plan' ELSE 'grab' END
      FROM json_each(NEW.wantables) j WHERE j.type = 'text';
END;

CREATE TRIGGER wantable_attempt_on_scope AFTER UPDATE OF wantables ON downloads
WHEN NEW.wantables != OLD.wantables
BEGIN
    INSERT OR REPLACE INTO wantable_attempts (wantable_id, download_id, decided_at, decided_by)
    SELECT j.value, NEW.id, CAST(strftime('%s','now') AS INTEGER) * 1000, 'scope'
      FROM json_each(NEW.wantables) j
     WHERE j.type = 'text' AND j.value NOT IN (SELECT value FROM json_each(OLD.wantables));
END;

-- 5. The ordered history reader's cursor (contract section 2.5). Written by
--    the poll's history reader alone, never by the event stream.
CREATE TABLE runner_history_cursors (
    client_id INTEGER PRIMARY KEY,
    incarnation TEXT NOT NULL DEFAULT '',
    since_seq INTEGER NOT NULL DEFAULT 0,
    head_seq INTEGER NOT NULL DEFAULT 0,
    retained_min_seq INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
) STRICT;

-- 6. Data migration (contract sections 3.1, 3.4, 6).
--
-- Rows already past their terminal keep it: a replayed history row or a
-- /deliveries row (now read with include_accepted=true) must be a no-op on
-- them, never a second import or a re-grab of an old failure.
UPDATE downloads SET runner_outcome = 'completed', runner_outcome_at = updated_at, runner_outcome_reason = 'legacy'
 WHERE state IN ('downloaded','importing','imported') AND parked_reason = '' AND superseded = 0;
UPDATE downloads SET runner_outcome = 'completed', runner_outcome_at = updated_at, runner_outcome_reason = 'legacy'
 WHERE state = 'awaiting_import' AND parked_reason = '' AND superseded = 0
   AND error NOT LIKE '%replacement held until cleanup is settled';
-- A pre-v2 failure keeps counting toward the re-grab budget as it did.
UPDATE downloads SET runner_outcome = 'failed', runner_outcome_at = updated_at, runner_outcome_reason = 'legacy',
       runner_blames_release = 1
 WHERE state = 'failed';

-- Live parked and superseded rows, and failures held "until cleanup is
-- settled", become provisional failures the operator sees once in Resolve; a
-- later Runner fact can still settle them.
UPDATE downloads SET
    state = 'failed', provisional = 1, runner_outcome = 'failed', runner_outcome_reason = 'parked', runner_blames_release = 0,
    runner_outcome_at = CAST(strftime('%s','now') AS INTEGER) * 1000,
    error = 'parked: ' || CASE
        WHEN parked_reason != '' THEN parked_reason
        WHEN superseded = 1 THEN 'superseded; ' || error
        ELSE error END,
    state_entered_at = CAST(strftime('%s','now') AS INTEGER) * 1000,
    updated_at = CAST(strftime('%s','now') AS INTEGER) * 1000
 WHERE ((parked_reason != '' OR superseded = 1) AND state NOT IN ('imported','failed'))
    OR (state = 'awaiting_import' AND error LIKE '%replacement held until cleanup is settled');

-- Rows Runner is holding are held, with the 24 h allowance contract section 6
-- gives a hold that carries no deadline of its own.
UPDATE downloads SET state = 'held',
    deadline_at = CAST(strftime('%s','now') AS INTEGER) * 1000 + 86400000,
    error = COALESCE(json_extract(runner_control, '$.message'), error)
 WHERE state = 'downloading' AND runner_control != '' AND json_valid(runner_control)
   AND json_extract(runner_control, '$.lifecycle') = 'held';
UPDATE downloads SET state = 'submitting',
    deadline_at = CAST(strftime('%s','now') AS INTEGER) * 1000 + 600000
 WHERE state = 'grabbed' AND submission_phase IN ('submitting','uncertain') AND request_token_idempotent = 1;
UPDATE downloads SET deadline_at = CAST(strftime('%s','now') AS INTEGER) * 1000 +
        CASE WHEN protocol = 'torrent' THEN 86400000 ELSE 1800000 END
 WHERE state = 'downloading' AND deadline_at = 0;
UPDATE downloads SET deadline_at = CAST(strftime('%s','now') AS INTEGER) * 1000 + 600000
 WHERE state = 'grabbed' AND deadline_at = 0;
UPDATE downloads SET deadline_at = CAST(strftime('%s','now') AS INTEGER) * 1000 + 21600000
 WHERE state IN ('downloaded','importing') AND deadline_at = 0;

-- Every local v1 receipt becomes a historical publication with the per-file
-- outcomes it had reached, matched to its download or not (contract section 6).
INSERT OR IGNORE INTO publications (download_id, generation, kind, path, remote_path, manifest_json, manifest_source,
                                    cross_volume, state, cohort, discovered_at, deadline_at, updated_at)
SELECT CASE WHEN r.download_id != 0 AND EXISTS (SELECT 1 FROM downloads d WHERE d.id = r.download_id) THEN r.download_id END,
       r.publication_generation,
       'completed',
       CASE WHEN r.mapped_path != '' THEN r.mapped_path ELSE r.publication_path END,
       r.publication_path,
       json_object('generation', r.publication_generation,
                   'digest', r.manifest_digest,
                   'delivery_id', r.delivery_id,
                   'files', (SELECT json_group_array(json_object('path', json_extract(f.value, '$.path'),
                                                                 'bytes', json_extract(f.value, '$.sizeBytes'),
                                                                 'sha256', json_extract(f.value, '$.sha256')))
                               FROM json_each(r.files_json) f)),
       'v1_record', 0,
       CASE r.state WHEN 'imported' THEN 'imported' ELSE 'discovered' END,
       'historical', r.ready_at, 0, CAST(strftime('%s','now') AS INTEGER) * 1000
  FROM runner_delivery_receipts r
 WHERE json_valid(r.files_json)
 ORDER BY r.id;

INSERT OR IGNORE INTO publication_files (publication_id, path, bytes, sha256, updated_at)
SELECT p.id, json_extract(f.value, '$.path'), COALESCE(json_extract(f.value, '$.bytes'), 0),
       COALESCE(json_extract(f.value, '$.sha256'), ''), p.updated_at
  FROM publications p, json_each(json_extract(p.manifest_json, '$.files')) f
 WHERE p.manifest_source = 'v1_record' AND json_extract(f.value, '$.path') IS NOT NULL;

UPDATE publication_files SET
    outcome = CASE (SELECT o.disposition FROM runner_delivery_file_outcomes o
                      JOIN runner_delivery_receipts r ON r.id = o.receipt_id
                     WHERE (CASE WHEN r.mapped_path != '' THEN r.mapped_path ELSE r.publication_path END) =
                           (SELECT path FROM publications WHERE id = publication_files.publication_id)
                       AND o.path = publication_files.path)
              WHEN 'placed' THEN 'placed' WHEN 'redundant' THEN 'discarded' ELSE 'pending' END,
    library_path = COALESCE((SELECT o.library_path FROM runner_delivery_file_outcomes o
                      JOIN runner_delivery_receipts r ON r.id = o.receipt_id
                     WHERE (CASE WHEN r.mapped_path != '' THEN r.mapped_path ELSE r.publication_path END) =
                           (SELECT path FROM publications WHERE id = publication_files.publication_id)
                       AND o.path = publication_files.path), ''),
    removed_at = COALESCE((SELECT CASE WHEN o.cleanup_state = 'removed' THEN o.updated_at ELSE 0 END
                      FROM runner_delivery_file_outcomes o
                      JOIN runner_delivery_receipts r ON r.id = o.receipt_id
                     WHERE (CASE WHEN r.mapped_path != '' THEN r.mapped_path ELSE r.publication_path END) =
                           (SELECT path FROM publications WHERE id = publication_files.publication_id)
                       AND o.path = publication_files.path), 0);
UPDATE publication_files SET placed_sha256 = sha256 WHERE outcome = 'placed';
UPDATE publication_files SET discard_reason = 'duplicate' WHERE outcome = 'discarded';

-- Precedence seed: the newest non-failed download per wantable. Two newest
-- with the same added_at are ambiguous and seed nothing, so their files are
-- unresolved, by design (contract section 3.4).
INSERT OR REPLACE INTO wantable_attempts (wantable_id, download_id, decided_at, decided_by)
SELECT w, id, added_at, 'migration' FROM (
    SELECT j.value AS w, d.id, d.added_at,
           RANK() OVER (PARTITION BY j.value ORDER BY d.added_at DESC) AS r,
           COUNT(*) OVER (PARTITION BY j.value, d.added_at) AS n
      FROM downloads d, json_each(d.wantables) j
     WHERE d.state != 'failed' AND j.type = 'text'
) WHERE r = 1 AND n = 1;

-- The historical cohort also covers directories present on disk at the first
-- M2 start; that is done once, in Go (markHistoricalCohort), not here.
COMMIT;
-- +goose StatementEnd

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

-- +goose StatementBegin
BEGIN;
DROP TRIGGER IF EXISTS wantable_attempt_on_insert;
DROP TRIGGER IF EXISTS wantable_attempt_on_scope;
DROP TABLE IF EXISTS runner_history_cursors;
DROP TABLE IF EXISTS wantable_attempts;
DROP TABLE IF EXISTS replacement_grabs;
DROP TABLE IF EXISTS publication_files;
DROP TABLE IF EXISTS publications;

CREATE TABLE downloads_v1 (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    copy_id       INTEGER,
    wantables     TEXT NOT NULL DEFAULT '[]',
    season        INTEGER NOT NULL DEFAULT -1,
    release_title TEXT NOT NULL,
    indexer       TEXT NOT NULL DEFAULT '',
    protocol      TEXT NOT NULL,
    quality       TEXT NOT NULL DEFAULT '',
    size          INTEGER NOT NULL DEFAULT 0,
    client_id     INTEGER NOT NULL DEFAULT 0,
    handle        TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'grabbed'
        CHECK (state IN ('planned','grabbed','downloading','downloaded','awaiting_import','importing','imported','failed')),
    progress      REAL NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT '',
    save_path     TEXT NOT NULL DEFAULT '',
    import_path   TEXT NOT NULL DEFAULT '',
    handoff_log   TEXT NOT NULL DEFAULT '[]',
    added_at      INTEGER NOT NULL,
    transfer TEXT NOT NULL DEFAULT '',
    payload_removed INTEGER NOT NULL DEFAULT 0,
    match_evidence TEXT NOT NULL DEFAULT '{}',
    runner_control TEXT NOT NULL DEFAULT '',
    plan_id INTEGER REFERENCES acquisition_plans(id),
    candidate_key TEXT NOT NULL DEFAULT '',
    submission_phase TEXT NOT NULL DEFAULT 'submitted' CHECK(submission_phase IN ('pending','submitting','submitted','uncertain','rejected')),
    execution_payload TEXT NOT NULL DEFAULT '{}',
    reserved_episodes TEXT NOT NULL DEFAULT '[]',
    parked_reason TEXT NOT NULL DEFAULT '',
    parked_at INTEGER NOT NULL DEFAULT 0,
    superseded INTEGER NOT NULL DEFAULT 0,
    observation TEXT NOT NULL DEFAULT '{}',
    cleanup_pending INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL,
    activity_dismissed INTEGER NOT NULL DEFAULT 0,
    generation_key TEXT NOT NULL DEFAULT '',
    intent TEXT NOT NULL DEFAULT 'automatic' CHECK (intent IN ('automatic','manual_additional')),
    request_id TEXT NOT NULL DEFAULT '',
    request_fingerprint TEXT NOT NULL DEFAULT '',
    blacklist_override INTEGER NOT NULL DEFAULT 0 CHECK (blacklist_override IN (0,1)),
    delivery_expected INTEGER NOT NULL DEFAULT 0 CHECK (delivery_expected IN (0,1)),
    request_token_idempotent INTEGER NOT NULL DEFAULT 0 CHECK (request_token_idempotent IN (0,1)),
    manual_retry_at INTEGER NOT NULL DEFAULT 0,
    manual_retry_attempts INTEGER NOT NULL DEFAULT 0,
    manual_blocked_at INTEGER NOT NULL DEFAULT 0,
    replacement_operation_id TEXT NOT NULL DEFAULT '',
    source_guid TEXT NOT NULL DEFAULT '',
    source_indexer_id INTEGER NOT NULL DEFAULT 0,
    runner_instance_id TEXT NOT NULL DEFAULT '',
    runner_admission_generation TEXT NOT NULL DEFAULT '',
    runner_submission_retry_at INTEGER NOT NULL DEFAULT 0,
    runner_submission_retry_attempts INTEGER NOT NULL DEFAULT 0,
    runner_submission_blocked_at INTEGER NOT NULL DEFAULT 0,
    provider_retry_after INTEGER NOT NULL DEFAULT 0,
    exact_transfer_correlation INTEGER NOT NULL DEFAULT 0 CHECK(exact_transfer_correlation IN (0,1))
) STRICT;
INSERT INTO downloads_v1
SELECT id, media_item_id, copy_id, wantables, season, release_title, indexer, protocol, quality, size,
    client_id, handle,
    CASE state WHEN 'held' THEN 'downloading' WHEN 'submitting' THEN 'grabbed' WHEN 'notified' THEN 'imported' ELSE state END,
    progress, error, save_path, import_path, handoff_log, added_at, transfer,
    payload_removed, match_evidence, runner_control, plan_id, candidate_key, submission_phase,
    execution_payload, reserved_episodes, parked_reason, parked_at, superseded, observation,
    cleanup_pending, updated_at, activity_dismissed, generation_key, intent, request_id,
    request_fingerprint, blacklist_override, delivery_expected, request_token_idempotent,
    manual_retry_at, manual_retry_attempts, manual_blocked_at, replacement_operation_id, source_guid,
    source_indexer_id, runner_instance_id, runner_admission_generation, runner_submission_retry_at,
    runner_submission_retry_attempts, runner_submission_blocked_at, provider_retry_after,
    exact_transfer_correlation
FROM downloads;
DROP TABLE downloads;
ALTER TABLE downloads_v1 RENAME TO downloads;
CREATE INDEX download_failure_window ON downloads(state,added_at);
CREATE UNIQUE INDEX download_plan_candidate ON downloads(plan_id,candidate_key) WHERE plan_id IS NOT NULL;
CREATE UNIQUE INDEX downloads_generation_key ON downloads(generation_key);
CREATE UNIQUE INDEX downloads_request_id ON downloads(request_id) WHERE request_id != '';
CREATE INDEX downloads_runner_mode_admission ON downloads(transfer,handle,runner_instance_id) WHERE runner_admission_generation!='';
CREATE INDEX idx_downloads_state ON downloads(state);
CREATE TRIGGER acquisition_download_terminal_custody AFTER UPDATE OF state ON downloads
WHEN NEW.state IN ('imported','failed') AND NEW.submission_phase NOT IN ('submitting','uncertain') AND NEW.parked_reason NOT LIKE 'operator cancellation%'
BEGIN
 UPDATE downloads SET parked_at=0,parked_reason=CASE WHEN cleanup_pending=0 THEN '' ELSE parked_reason END WHERE id=NEW.id;
END;
CREATE TRIGGER completed_receipt_update AFTER UPDATE ON downloads
WHEN NEW.import_path != ''
BEGIN
    INSERT INTO completed_receipts
        (download_id, added_at, path, client_id, title, state, protocol,
         payload_removed, generation_key)
    VALUES (NEW.id, NEW.added_at, NEW.import_path, NEW.client_id, NEW.release_title,
            NEW.state, NEW.protocol, NEW.payload_removed, NEW.generation_key)
    ON CONFLICT(generation_key, path) DO UPDATE SET
        added_at=excluded.added_at, client_id=excluded.client_id,
        title=excluded.title, state=excluded.state, protocol=excluded.protocol,
        payload_removed=excluded.payload_removed;
END;
CREATE TRIGGER download_generation_immutable BEFORE UPDATE OF generation_key ON downloads
WHEN OLD.generation_key != '' AND NEW.generation_key != OLD.generation_key
BEGIN
    SELECT RAISE(ABORT, 'download generation key is immutable');
END;
CREATE TRIGGER download_generation_insert AFTER INSERT ON downloads
BEGIN
    UPDATE downloads
       SET generation_key = CASE WHEN NEW.generation_key = ''
                                 THEN lower(hex(randomblob(16)))
                                 ELSE NEW.generation_key END
     WHERE id = NEW.id;
    INSERT INTO completed_receipts
        (download_id, added_at, path, client_id, title, state, protocol,
         payload_removed, generation_key)
    SELECT id, added_at, import_path, client_id, release_title, state, protocol,
           payload_removed, generation_key
      FROM downloads WHERE id = NEW.id AND import_path != ''
    ON CONFLICT(generation_key, path) DO UPDATE SET
        added_at=excluded.added_at, client_id=excluded.client_id,
        title=excluded.title, state=excluded.state, protocol=excluded.protocol,
        payload_removed=excluded.payload_removed;
END;
CREATE TRIGGER prevent_cancelled_import_delete BEFORE DELETE ON downloads
WHEN EXISTS(SELECT 1 FROM import_attempt_controls c WHERE c.download_id=OLD.id AND c.outcome IN ('pending','cancelled'))
 OR EXISTS(SELECT 1 FROM import_attempt_stages s WHERE s.download_id=OLD.id AND s.state='held')
 OR EXISTS(SELECT 1 FROM import_placements p WHERE json_extract(p.data,'$.download_id')=OLD.id AND p.state IN ('prepared','retained'))
 OR EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.download_id=OLD.id AND (a.outcome='pending' OR a.resolution='retained_for_review'))
BEGIN
    SELECT RAISE(ABORT,'download retains import cancellation or publication custody');
END;
CREATE TRIGGER prevent_download_delete_with_runner_delivery
BEFORE DELETE ON downloads
WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts r
    WHERE r.download_id=OLD.id AND (r.state!='imported' OR r.ack_state!='accepted'))
BEGIN
    SELECT RAISE(ABORT, 'download has unresolved Runner delivery custody');
END;
COMMIT;
-- +goose StatementEnd

PRAGMA foreign_keys = ON;
