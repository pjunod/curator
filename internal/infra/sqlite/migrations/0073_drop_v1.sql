-- +goose Up
-- Pipeline contract v2, M4: cleanup is per publication file (contract section
-- 3.4), so the v1 mechanisms it replaces lose their tables. Migrate.go refuses
-- to run this file while an operator's v1 deletion request is still pending
-- (runner_delivery_explicit_delete_*), naming its batch ids: dropping it would
-- silently forget a decision a person made.
--
-- Kept, deliberately (ADR 0026 addendum, STATUS M4): downloads.parked_reason,
-- parked_at, superseded, delivery_expected, request_token_idempotent and
-- exact_transfer_correlation. M2 kept live code on each (the season planner's
-- uncertain-submission fence and operator cancellation for every client, the
-- Runner delivery request parameter and native-torrent precondition, the
-- submission retry, qBittorrent tag correlation), so none of them is a dead
-- column yet. runner_delivery_receipts and runner_delivery_file_outcomes stay
-- read-only for one more release: the v1 adapter still reads them (M6 drops).

-- 1. Storage-change review fences that name a path rather than a generation
--    (an orphan payload on the replaced volume) move out of the 0040 cohort
--    table into their own. The storage-identity check is what stays.
CREATE TABLE storage_change_review_paths (
    review_token TEXT NOT NULL,
    path TEXT NOT NULL,
    PRIMARY KEY (review_token, path)
) STRICT;
INSERT OR IGNORE INTO storage_change_review_paths (review_token, path)
SELECT substr(generation_key, length('storage-change:') + 1), path
  FROM legacy_cleanup_review
 WHERE generation_key LIKE 'storage-change:%' AND path != '';

-- 2. The migration-0040 review cohort folds into the historical cohort (M2):
--    every path it fenced that no publication claims yet becomes a historical
--    publication, so nothing deletes it until the M7 review approves it. This
--    covers the cohort's generation-only entries too, through the path their
--    download row carries now. A review fence is replaced; nothing is freed.
INSERT OR IGNORE INTO publications (download_id, generation, kind, path, manifest_json, manifest_source, cross_volume, state, cohort, error, discovered_at, deadline_at, updated_at)
SELECT NULL, '', 'completed', p.path, '{}', 'none', 0, 'discovered', 'historical',
       'pre-delivery acquisition (migration 0040 review cohort); awaiting the M7 review',
       CAST(strftime('%s','now') AS INTEGER) * 1000, 0, CAST(strftime('%s','now') AS INTEGER) * 1000
  FROM (
        SELECT l.path AS path FROM legacy_cleanup_review l
         WHERE l.path != '' AND l.generation_key NOT LIKE 'storage-change:%'
        UNION
        SELECT d.import_path FROM downloads d
          JOIN legacy_cleanup_review l ON l.generation_key = d.generation_key
         WHERE d.import_path != '' AND l.generation_key NOT LIKE 'storage-change:%'
       ) p
 WHERE NOT EXISTS (SELECT 1 FROM publications q WHERE q.path = p.path);

DROP TRIGGER IF EXISTS legacy_inflight_import_evidence_immutable;
DROP TABLE legacy_inflight_import_evidence;
DROP INDEX IF EXISTS legacy_cleanup_review_path;
DROP TABLE legacy_cleanup_review;

-- 3. The deliveries feed cursor joins the history cursor's row; the v1 cursor
--    table goes. history_read says whether the history half was ever written
--    by the ordered history reader (a row made only for the deliveries cursor
--    must still start with continuity reconciliation).
ALTER TABLE runner_history_cursors ADD COLUMN deliveries_after INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_history_cursors ADD COLUMN history_read INTEGER NOT NULL DEFAULT 1 CHECK (history_read IN (0,1));
INSERT INTO runner_history_cursors (client_id, incarnation, since_seq, head_seq, retained_min_seq, updated_at, deliveries_after, history_read)
SELECT c.client_id, '', 0, 0, 0, c.updated_at, c.after_sequence, 0 FROM runner_delivery_cursors c WHERE true
ON CONFLICT (client_id) DO UPDATE SET deliveries_after = excluded.deliveries_after;
DROP TABLE runner_delivery_cursors;

-- 4. The receipt-bound delete guard on downloads goes with the receipt-bound
--    cleanup: every receipt is a publication since 0072, and a row whose
--    publication still has files on disk is never deleted (Activity retention
--    and Clear test that instead, contract section 3.4).
DROP TRIGGER IF EXISTS prevent_download_delete_with_runner_delivery;

-- 5. Unused since M2 (admission-poll pacing) and since this milestone
--    (receipt-bound explicit deletion batches).
DROP TABLE runner_admission_poll_schedule;
DROP INDEX IF EXISTS idx_delivery_explicit_delete_receipt;
DROP TABLE runner_delivery_explicit_delete_files;
DROP TABLE runner_delivery_explicit_delete_batches;

-- +goose Down
-- The dropped mechanisms are not restored: their tables come back empty so an
-- older build starts, and the folded cohort stays historical (safe: it only
-- prevents deletion).
CREATE TABLE runner_delivery_explicit_delete_batches (
    batch_id TEXT PRIMARY KEY,
    receipt_id INTEGER NOT NULL REFERENCES runner_delivery_receipts(id),
    payload_path TEXT NOT NULL,
    inventory_fingerprint TEXT NOT NULL,
    publication_directory_identity TEXT NOT NULL,
    storage_identity TEXT NOT NULL,
    generation_key TEXT NOT NULL,
    request_token TEXT NOT NULL,
    publication_generation TEXT NOT NULL,
    retry_attempts INTEGER NOT NULL DEFAULT 0,
    retry_next_attempt_at INTEGER NOT NULL DEFAULT 0,
    retry_blocked_since INTEGER NOT NULL DEFAULT 0,
    retry_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    abandoned_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(receipt_id, payload_path, inventory_fingerprint, batch_id)
) STRICT;
CREATE TABLE runner_delivery_explicit_delete_files (
    batch_id TEXT NOT NULL REFERENCES runner_delivery_explicit_delete_batches(batch_id),
    path TEXT NOT NULL,
    reviewed_path TEXT NOT NULL,
    snapshot_identity TEXT NOT NULL,
    parent_identity TEXT NOT NULL,
    history_recorded INTEGER NOT NULL DEFAULT 0 CHECK (history_recorded IN (0,1)),
    completed_at INTEGER NOT NULL DEFAULT 0,
    unlink_started INTEGER NOT NULL DEFAULT 0 CHECK(unlink_started IN (0,1)),
    PRIMARY KEY(batch_id, path)
) STRICT;
CREATE INDEX idx_delivery_explicit_delete_receipt
    ON runner_delivery_explicit_delete_batches(receipt_id, retry_next_attempt_at, created_at);
CREATE TABLE runner_admission_poll_schedule (
    download_id INTEGER PRIMARY KEY REFERENCES downloads(id) ON DELETE CASCADE,
    generation_key TEXT NOT NULL,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE runner_delivery_cursors (client_id INTEGER PRIMARY KEY, after_sequence INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL) STRICT;
INSERT INTO runner_delivery_cursors (client_id, after_sequence, updated_at)
SELECT client_id, deliveries_after, updated_at FROM runner_history_cursors;
ALTER TABLE runner_history_cursors DROP COLUMN deliveries_after;
DELETE FROM runner_history_cursors WHERE history_read = 0;
ALTER TABLE runner_history_cursors DROP COLUMN history_read;
CREATE TABLE legacy_cleanup_review (
    generation_key TEXT NOT NULL,
    path TEXT NOT NULL,
    PRIMARY KEY (generation_key, path)
);
CREATE INDEX legacy_cleanup_review_path ON legacy_cleanup_review(path);
INSERT OR IGNORE INTO legacy_cleanup_review (generation_key, path)
SELECT 'storage-change:' || review_token, path FROM storage_change_review_paths;
-- The folded cohort keeps fencing an older build, and keeps 0040's own
-- downgrade guard meaningful.
INSERT OR IGNORE INTO legacy_cleanup_review (generation_key, path)
SELECT 'm4-historical:' || id, path FROM publications
 WHERE cohort = 'historical' AND error LIKE 'pre-delivery acquisition (migration 0040 review cohort)%';
-- +goose StatementBegin
CREATE TRIGGER prevent_download_delete_with_runner_delivery
BEFORE DELETE ON downloads
WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts r
    WHERE r.download_id=OLD.id AND (r.state!='imported' OR r.ack_state!='accepted'))
BEGIN
    SELECT RAISE(ABORT, 'download has unresolved Runner delivery custody');
END;
-- +goose StatementEnd
CREATE TABLE legacy_inflight_import_evidence (
 generation_key TEXT PRIMARY KEY,
 cutover_ms INTEGER NOT NULL,
 evidence TEXT NOT NULL
) STRICT;
-- +goose StatementBegin
CREATE TRIGGER legacy_inflight_import_evidence_immutable BEFORE UPDATE ON legacy_inflight_import_evidence
BEGIN SELECT RAISE(ABORT,'legacy in-flight import evidence is immutable'); END;
-- +goose StatementEnd
DROP TABLE storage_change_review_paths;
