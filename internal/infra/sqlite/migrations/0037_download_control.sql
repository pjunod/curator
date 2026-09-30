-- +goose Up
ALTER TABLE downloads ADD COLUMN runner_control TEXT NOT NULL DEFAULT '';

-- +goose Down
-- A downgrade must not erase unresolved custody. Refuse while holds exist.
-- +goose StatementBegin
CREATE TEMP TABLE control_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO control_downgrade_guard SELECT COUNT(*) FROM downloads WHERE runner_control != '';
DROP TABLE control_downgrade_guard;
-- +goose StatementEnd
ALTER TABLE downloads DROP COLUMN runner_control;
