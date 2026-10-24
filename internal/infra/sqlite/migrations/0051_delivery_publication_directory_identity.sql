-- +goose Up
-- Bind directory-level cleanup to the accepted publication directory, even
-- after Curator has removed the manifest files and can no longer re-hash them.
-- The value is an inode identity paired with the verified storage source.
ALTER TABLE runner_delivery_receipts
    ADD COLUMN publication_directory_identity TEXT NOT NULL DEFAULT '';

-- +goose Down
CREATE TEMP TABLE delivery_directory_identity_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO delivery_directory_identity_downgrade_guard
    SELECT CASE WHEN EXISTS (SELECT 1 FROM runner_delivery_receipts) THEN 0 ELSE 1 END;
ALTER TABLE runner_delivery_receipts DROP COLUMN publication_directory_identity;
DROP TABLE delivery_directory_identity_downgrade_guard;
