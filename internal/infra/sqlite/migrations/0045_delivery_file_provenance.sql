-- +goose Up
-- Preserve the immutable Runner source generation alongside the exact
-- Curator placement and replacement operation that consumed it.
CREATE TABLE runner_delivery_file_imports (
    id INTEGER PRIMARY KEY,
    receipt_id INTEGER NOT NULL REFERENCES runner_delivery_receipts(id),
    path TEXT NOT NULL,
    source_generation TEXT NOT NULL DEFAULT '',
    source_transfer TEXT NOT NULL DEFAULT '',
    source_payload_generation TEXT NOT NULL DEFAULT '',
    replacement_operation_id TEXT NOT NULL DEFAULT '',
    replacement_disposition TEXT NOT NULL DEFAULT ''
        CHECK (replacement_disposition IN ('','selected','excess')),
    placement_id TEXT NOT NULL DEFAULT '',
    library_file_generation TEXT NOT NULL DEFAULT '',
    library_file_identity TEXT NOT NULL DEFAULT '',
    library_path TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE(receipt_id,path,replacement_operation_id,placement_id,library_file_generation)
) STRICT;

-- +goose Down
-- Delivery generations and replacement placement evidence are audit records.
-- Refuse downgrade while any delivery receipt exists rather than erasing it.
CREATE TEMP TABLE delivery_file_provenance_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO delivery_file_provenance_downgrade_guard
    SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_delivery_file_outcomes)
        OR EXISTS (SELECT 1 FROM runner_delivery_receipts)
        THEN 0 ELSE 1 END;
DROP TABLE runner_delivery_file_imports;
DROP TABLE delivery_file_provenance_downgrade_guard;
