-- +goose Up
CREATE TABLE import_attempt_controls (
    download_id INTEGER NOT NULL REFERENCES downloads(id) ON DELETE CASCADE,
    generation_key TEXT NOT NULL,
    attempt_epoch INTEGER NOT NULL DEFAULT 0 CHECK(attempt_epoch>=0),
    outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','pending','cancelled','completed')),
    requested_at INTEGER,
    finalized_at INTEGER,
    plan_sealed INTEGER NOT NULL DEFAULT 0 CHECK(plan_sealed IN (0,1)),
    PRIMARY KEY(download_id,generation_key),
    CHECK ((outcome='' AND requested_at IS NULL AND finalized_at IS NULL)
        OR (outcome='pending' AND requested_at IS NOT NULL AND finalized_at IS NULL)
        OR (outcome='cancelled' AND requested_at IS NOT NULL AND finalized_at IS NOT NULL)
        OR (outcome='completed' AND finalized_at IS NOT NULL))
) STRICT;

CREATE TABLE import_attempt_files (
    download_id INTEGER NOT NULL,
    generation_key TEXT NOT NULL,
    attempt_epoch INTEGER NOT NULL CHECK(attempt_epoch>=0),
    source TEXT NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('media','sidecar')),
    outcome TEXT NOT NULL DEFAULT 'pending' CHECK(outcome IN ('pending','committed','skipped')),
    placement_id TEXT,
    PRIMARY KEY(download_id,generation_key,attempt_epoch,source),
    FOREIGN KEY(download_id,generation_key) REFERENCES import_attempt_controls(download_id,generation_key) ON DELETE CASCADE
) STRICT;

CREATE TABLE import_attempt_stages (
 download_id INTEGER NOT NULL,
 generation_key TEXT NOT NULL,
 attempt_epoch INTEGER NOT NULL,
 stage TEXT NOT NULL PRIMARY KEY,
 source TEXT NOT NULL,
 target TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'held' CHECK(state IN ('held','released','operator_disposed')),
 FOREIGN KEY(download_id,generation_key) REFERENCES import_attempt_controls(download_id,generation_key) ON DELETE CASCADE
) STRICT;

CREATE TABLE import_publication_authorizations (
    download_id INTEGER NOT NULL,
    generation_key TEXT NOT NULL,
    attempt_epoch INTEGER NOT NULL CHECK(attempt_epoch>=0),
    publication_id TEXT NOT NULL,
    placement_id TEXT REFERENCES import_placements(id) ON DELETE RESTRICT,
    receipt_id INTEGER,
    source TEXT NOT NULL,
    target TEXT NOT NULL,
    payload TEXT NOT NULL CHECK(json_valid(payload)),
    authorized_at INTEGER NOT NULL,
    outcome TEXT NOT NULL DEFAULT 'pending' CHECK(outcome IN ('pending','committed','reconciled')),
    resolved_at INTEGER,
    resolution TEXT NOT NULL DEFAULT '' CHECK(resolution IN ('','retained_for_review','operator_adopted','operator_disposed')),
    PRIMARY KEY(download_id,generation_key,attempt_epoch,publication_id),
    UNIQUE(download_id,generation_key,publication_id),
    FOREIGN KEY(download_id,generation_key) REFERENCES import_attempt_controls(download_id,generation_key) ON DELETE CASCADE,
    CHECK ((outcome='pending' AND resolved_at IS NULL AND resolution='')
        OR (outcome='committed' AND resolved_at IS NOT NULL AND resolution='')
        OR (outcome='reconciled' AND resolved_at IS NOT NULL AND resolution!=''))
) STRICT;
CREATE INDEX idx_import_publication_review ON import_publication_authorizations(target,resolution);
DROP INDEX import_placement_download;
CREATE INDEX import_placement_download ON import_placements(json_extract(data,'$.download_id')) WHERE state IN ('prepared','committed','retained');

-- +goose StatementBegin
CREATE TRIGGER prevent_cancelled_import_delete BEFORE DELETE ON downloads
WHEN EXISTS(SELECT 1 FROM import_attempt_controls c WHERE c.download_id=OLD.id AND c.outcome IN ('pending','cancelled'))
 OR EXISTS(SELECT 1 FROM import_attempt_stages s WHERE s.download_id=OLD.id AND s.state='held')
 OR EXISTS(SELECT 1 FROM import_placements p WHERE json_extract(p.data,'$.download_id')=OLD.id AND p.state IN ('prepared','retained'))
 OR EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.download_id=OLD.id AND (a.outcome='pending' OR a.resolution='retained_for_review'))
BEGIN
    SELECT RAISE(ABORT,'download retains import cancellation or publication custody');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER prevent_cancelled_delivery_receipt_delete BEFORE DELETE ON runner_delivery_receipts
WHEN EXISTS(SELECT 1 FROM import_attempt_controls c WHERE c.download_id=OLD.download_id AND c.generation_key=OLD.generation_key AND c.outcome IN ('pending','cancelled'))
 OR EXISTS(SELECT 1 FROM import_attempt_stages s WHERE s.download_id=OLD.download_id AND s.generation_key=OLD.generation_key AND s.state='held')
 OR EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.receipt_id=OLD.id AND (a.outcome='pending' OR a.resolution='retained_for_review'))
BEGIN
    SELECT RAISE(ABORT,'delivery retains import publication custody');
END;
-- +goose StatementEnd

-- +goose Down
-- Old binaries ignore these fences; never silently lose effective intent or
-- unresolved/revoked publication custody on rollback.
-- +goose StatementBegin
CREATE TEMP TABLE import_cancellation_downgrade_guard(n INTEGER CHECK(n=0));
INSERT INTO import_cancellation_downgrade_guard SELECT COUNT(*) FROM import_attempt_controls WHERE outcome IN ('pending','cancelled');
INSERT INTO import_cancellation_downgrade_guard SELECT COUNT(*) FROM import_publication_authorizations WHERE outcome='pending' OR resolution IN ('retained_for_review','operator_adopted');
INSERT INTO import_cancellation_downgrade_guard SELECT COUNT(*) FROM import_placements WHERE state='retained' OR (state='prepared' AND json_extract(data,'$.import_attempt_epoch') IS NOT NULL);
INSERT INTO import_cancellation_downgrade_guard SELECT COUNT(*) FROM import_attempt_stages WHERE state='held';
DROP TABLE import_cancellation_downgrade_guard;
-- +goose StatementEnd
DROP INDEX import_placement_download;
CREATE INDEX import_placement_download ON import_placements(json_extract(data,'$.download_id')) WHERE state IN ('prepared','committed');
DROP TRIGGER prevent_cancelled_delivery_receipt_delete;
DROP TRIGGER prevent_cancelled_import_delete;
DROP TABLE import_attempt_stages;
DROP TABLE import_publication_authorizations;
DROP TABLE import_attempt_files;
DROP TABLE import_attempt_controls;
