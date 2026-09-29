-- +goose Up
-- Activity retention must not erase the explanation for bytes still on disk.
-- These receipts deliberately have no foreign keys to disposable queue rows.
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
INSERT INTO completed_receipts
SELECT id, added_at, import_path, client_id, release_title, state, protocol, payload_removed
FROM downloads WHERE import_path != '';

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

CREATE TABLE completed_scans (
    root TEXT PRIMARY KEY,
    report TEXT NOT NULL
);

CREATE TABLE completed_cleanup_attempts (
    download_id INTEGER PRIMARY KEY,
    tried_at INTEGER NOT NULL
);

-- +goose Down
DROP TRIGGER completed_receipt_update;
DROP TRIGGER completed_receipt_insert;
DROP TABLE completed_cleanup_attempts;
DROP TABLE completed_scans;
DROP TABLE completed_receipts;
