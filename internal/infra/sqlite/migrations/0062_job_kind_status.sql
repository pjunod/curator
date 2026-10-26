-- +goose Up
-- Status polling must not scan every probe/adoption job to find the last scan.
CREATE INDEX idx_jobs_kind_id ON jobs (kind, id DESC);

-- +goose Down
DROP INDEX idx_jobs_kind_id;
