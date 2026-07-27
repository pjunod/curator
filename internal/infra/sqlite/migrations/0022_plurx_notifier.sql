-- +goose Up
-- A 'plurx' notifier type.
--
-- Numbered 22, not 21: 21 is a registered Go migration (migration0021.go)
-- that repairs a database where two branches both claimed 20. Read
-- migrations/README.md before adding another — the number is the only thing
-- goose compares.
--
-- plurx sits beside plex and jellyfin in the dispatcher's "media server"
-- group — import events only, never chatter — but it is not a refresh poke.
-- It receives the paths that landed and the ids Monarr already knows, and
-- plurx indexes exactly those. The type is separate because the settings
-- differ (a scoped `plx_` key, never an admin token) and because an operator
-- switching a plex notifier to plurx should be an explicit choice.
--
-- SQLite cannot alter a CHECK, so notifiers is rebuilt — same shape as 0007,
-- 0012 and 0020. Nothing has a foreign key to notifiers, so ids survive the
-- copy and no child rows need re-pointing.

CREATE TABLE notifiers_new (
    id        INTEGER PRIMARY KEY,
    type      TEXT NOT NULL CHECK (type IN ('webhook', 'discord', 'plex', 'jellyfin', 'plurx')),
    name      TEXT NOT NULL,
    settings  TEXT NOT NULL DEFAULT '{}',
    on_grab   INTEGER NOT NULL DEFAULT 1,
    on_import INTEGER NOT NULL DEFAULT 1,
    on_failed INTEGER NOT NULL DEFAULT 1,
    on_health INTEGER NOT NULL DEFAULT 0,
    enabled   INTEGER NOT NULL DEFAULT 1
) STRICT;

INSERT INTO notifiers_new
    (id, type, name, settings, on_grab, on_import, on_failed, on_health, enabled)
SELECT id, type, name, settings, on_grab, on_import, on_failed, on_health, enabled
FROM notifiers;

DROP TABLE notifiers;
ALTER TABLE notifiers_new RENAME TO notifiers;
