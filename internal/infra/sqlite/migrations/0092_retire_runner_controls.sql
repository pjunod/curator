-- +goose Up
-- Runner controls describe the live transfer. Handoff and Activity events retain
-- the hold history after the transfer has ended, including on legacy clients.
UPDATE downloads SET runner_control = ''
WHERE runner_control != '' AND (runner_outcome != '' OR state IN ('downloaded','awaiting_import','importing','imported','notified','failed'));

-- +goose StatementBegin
CREATE TRIGGER retire_runner_control AFTER UPDATE OF state, runner_outcome, runner_control ON downloads
WHEN NEW.runner_control != '' AND (NEW.runner_outcome != '' OR NEW.state IN ('downloaded','awaiting_import','importing','imported','notified','failed'))
BEGIN
    UPDATE downloads SET runner_control = '' WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER retire_runner_control;
