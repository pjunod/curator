-- +goose Up
-- A generation is immutable even when integer queue IDs, timestamps, titles,
-- handles, or paths are reused or updated during recovery.
ALTER TABLE downloads ADD COLUMN generation_key TEXT NOT NULL DEFAULT '';
UPDATE downloads SET generation_key = lower(hex(randomblob(16))) WHERE generation_key = '';
CREATE UNIQUE INDEX downloads_generation_key ON downloads(generation_key);

-- Receipts survive queue row deletion. Rebuild their key around generation and
-- path so a later acquisition reusing every old visible field cannot replace
-- the historical custody record.
DROP TRIGGER completed_receipt_insert;
DROP TRIGGER completed_receipt_update;
ALTER TABLE completed_receipts ADD COLUMN generation_key TEXT NOT NULL DEFAULT '';
UPDATE completed_receipts
SET generation_key = coalesce(
    (SELECT d.generation_key FROM downloads d
     WHERE d.id = completed_receipts.download_id
       AND d.added_at = completed_receipts.added_at
       AND d.import_path = completed_receipts.path
       AND d.client_id = completed_receipts.client_id
       AND d.release_title = completed_receipts.title
       AND d.protocol = completed_receipts.protocol),
    lower(hex(randomblob(16)))
)
WHERE generation_key = '';
ALTER TABLE completed_receipts RENAME TO completed_receipts_old;
CREATE TABLE completed_receipts (
    download_id INTEGER NOT NULL,
    added_at INTEGER NOT NULL,
    path TEXT NOT NULL,
    client_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    protocol TEXT NOT NULL,
    payload_removed INTEGER NOT NULL,
    generation_key TEXT NOT NULL,
    PRIMARY KEY (generation_key, path)
);
INSERT INTO completed_receipts
    (download_id, added_at, path, client_id, title, state, protocol,
     payload_removed, generation_key)
SELECT download_id, added_at, path, client_id, title, state, protocol,
       payload_removed, generation_key
FROM completed_receipts_old;
DROP TABLE completed_receipts_old;

-- Existing SQLite/raw inserts that omit the key receive a DB-minted identity.
-- The insert trigger updates the row before recording a receipt. Existing
-- generations can never be changed by ordinary row updates.
-- +goose StatementBegin
CREATE TRIGGER download_generation_immutable BEFORE UPDATE OF generation_key ON downloads
WHEN OLD.generation_key != '' AND NEW.generation_key != OLD.generation_key
BEGIN
    SELECT RAISE(ABORT, 'download generation key is immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
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
-- +goose StatementEnd
-- +goose StatementBegin
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
-- +goose StatementEnd

-- This table is intentionally independent of Activity and receipts. It is the
-- durable review fence for every generation/path that existed at migration.
CREATE TABLE legacy_cleanup_review (
    generation_key TEXT NOT NULL,
    path TEXT NOT NULL,
    PRIMARY KEY (generation_key, path)
);
INSERT OR IGNORE INTO legacy_cleanup_review(generation_key, path)
SELECT generation_key, import_path FROM downloads;
INSERT OR IGNORE INTO legacy_cleanup_review(generation_key, path)
SELECT generation_key, path FROM completed_receipts;
CREATE INDEX legacy_cleanup_review_path ON legacy_cleanup_review(path);

-- +goose Down
-- Do not discard legacy custody or merge separate generations into the old
-- receipt key. Operators must review/remove the cohort and settle ambiguous
-- duplicate obligations before applying this downgrade.
-- +goose StatementBegin
CREATE TEMP TABLE legacy_cleanup_downgrade_guard(n INTEGER CHECK(n=0));
INSERT INTO legacy_cleanup_downgrade_guard
SELECT (SELECT count(*) FROM legacy_cleanup_review)
     + (SELECT count(*) FROM (
            SELECT download_id, added_at, path
            FROM completed_receipts
            GROUP BY download_id, added_at, path
            HAVING count(*) > 1
        ));
DROP TABLE legacy_cleanup_downgrade_guard;
-- +goose StatementEnd
DROP INDEX legacy_cleanup_review_path;
DROP TABLE legacy_cleanup_review;
DROP TRIGGER completed_receipt_update;
DROP TRIGGER download_generation_immutable;
DROP TRIGGER download_generation_insert;
DROP INDEX downloads_generation_key;
ALTER TABLE completed_receipts RENAME TO completed_receipts_v0040;
CREATE TABLE completed_receipts (
    download_id INTEGER NOT NULL,
    added_at INTEGER NOT NULL,
    path TEXT NOT NULL,
    client_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    state TEXT NOT NULL,
    protocol TEXT NOT NULL,
    payload_removed INTEGER NOT NULL,
    PRIMARY KEY (download_id, added_at, path)
);
INSERT OR REPLACE INTO completed_receipts
    (download_id, added_at, path, client_id, title, state, protocol, payload_removed)
SELECT download_id, added_at, path, client_id, title, state, protocol, payload_removed
FROM completed_receipts_v0040;
DROP TABLE completed_receipts_v0040;
ALTER TABLE downloads DROP COLUMN generation_key;
-- +goose StatementBegin
CREATE TRIGGER completed_receipt_insert AFTER INSERT ON downloads
WHEN NEW.import_path != ''
BEGIN
    INSERT OR REPLACE INTO completed_receipts
    VALUES (NEW.id, NEW.added_at, NEW.import_path, NEW.client_id, NEW.release_title,
            NEW.state, NEW.protocol, NEW.payload_removed);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER completed_receipt_update AFTER UPDATE ON downloads
WHEN NEW.import_path != ''
BEGIN
    INSERT OR REPLACE INTO completed_receipts
    VALUES (NEW.id, NEW.added_at, NEW.import_path, NEW.client_id, NEW.release_title,
            NEW.state, NEW.protocol, NEW.payload_removed);
END;
-- +goose StatementEnd
