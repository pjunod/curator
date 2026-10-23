-- +goose Up
ALTER TABLE downloads ADD COLUMN exact_transfer_correlation INTEGER NOT NULL DEFAULT 0 CHECK(exact_transfer_correlation IN (0,1));

-- +goose Down
CREATE TEMP TABLE exact_transfer_correlation_downgrade_guard (
    ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO exact_transfer_correlation_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM downloads WHERE exact_transfer_correlation=1) THEN 0 ELSE 1 END;
ALTER TABLE downloads DROP COLUMN exact_transfer_correlation;
DROP TABLE exact_transfer_correlation_downgrade_guard;
