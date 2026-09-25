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

-- +goose Down
DROP TABLE recovery_receipt_outbox;
DROP TABLE recovery_file_results;
DROP TABLE recovery_imports;
DROP TABLE import_placements;
