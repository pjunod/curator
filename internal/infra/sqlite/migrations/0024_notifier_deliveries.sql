-- +goose Up
-- A delivery log for notifications, and the retry that needs it.
--
-- Delivery was one in-process attempt with a 20 s timeout and nothing
-- written down: a plurx that happened to be restarting missed the scan, and
-- the only trace was a Warn line in whatever log stream was being collected
-- that day. For chat notifiers that is survivable — a missed Discord message
-- is a missed Discord message. For plurx it is not: the missed message IS
-- the file never appearing in the library.
--
-- So deliveries are rows. A row survives a restart, which is the whole
-- point: the common failure is that both applications restart together
-- after a host reboot, and an in-memory retry loop dies with the process
-- that owns it.
--
-- `next_at` is when this row becomes due, so the backoff (5 s / 30 s / 2 m,
-- plan §3.6) is durable rather than a sleeping goroutine. `result` holds
-- what the far side answered on success — for plurx, the item it made —
-- which is the half of the story a `last_error` column cannot hold.
--
-- Numbered 24, and it took two goes. 21 is the Go repair migration, 22 added
-- the plurx notifier type, and 23 was already claimed on main by
-- `0023_file_provenance_release.sql` from another branch — exactly the
-- collision README.md was written about, walked into by the person who wrote
-- it. Worth recording what saved it: two files with the same number in ONE
-- directory make goose panic at startup, loudly. The silent, unrecoverable
-- case is only when the two never meet on disk — separate branches, one
-- database. Check `git log --all` before claiming a number, not `ls`.

CREATE TABLE notifier_deliveries (
    id          INTEGER PRIMARY KEY,
    notifier_id INTEGER NOT NULL,
    download_id INTEGER NOT NULL DEFAULT 0,
    event       TEXT NOT NULL,
    payload     TEXT NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'ok', 'failed')),
    next_at     INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

-- The worker's only query: what is due now.
CREATE INDEX idx_deliveries_due ON notifier_deliveries (status, next_at);
-- The UI's: this notifier's recent deliveries, newest first.
CREATE INDEX idx_deliveries_notifier ON notifier_deliveries (notifier_id, id DESC);
