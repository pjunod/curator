-- +goose Up
-- Watch state pushed from plurx (master plan §11.1).
--
-- Numbered 26, and 25 was the second collision this branch has had. It was
-- written as 25 after checking `git log --all`, and 25 was claimed on main
-- before this merged — which is the case the README is really about: the
-- number you check is a snapshot, and main keeps moving. Re-check at merge
-- time, not only at write time. (The first collision, 23, is in the README.)
--
-- Per-user, with usernames, by an explicit decision recorded in
-- docs/plan-integration.md §11.1. The sketch was an aggregate any-user
-- signal; per-user was chosen because it is the only shape that can later
-- mean "everyone who could has watched it", and an aggregate cannot be
-- refined into a per-user one after the fact.
--
-- That decision has a cost, and it shapes this table: these rows are other
-- people's viewing history, held by an application whose job is downloading.
-- So they are deliberately thin — who, what, when, and nothing else. No
-- position, no device, no duration, no history of every time somebody
-- rewatched something. `username` is plurx's, not a monarr account: monarr
-- has no notion of these people and should not grow one.
--
-- ON CONFLICT REPLACE on the natural key: watching something twice is one
-- fact with a newer date, not two rows. That also bounds the table by the
-- library size rather than by how much television gets watched.

CREATE TABLE plurx_watched (
    id            INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    -- plurx's username. Empty is allowed: a future aggregate sender, or a
    -- payload that omits it, records the fact without inventing a person.
    username      TEXT NOT NULL DEFAULT '',
    -- 0 for a movie; the episode's numbers for a series.
    season        INTEGER NOT NULL DEFAULT 0,
    episode       INTEGER NOT NULL DEFAULT 0,
    watched_at    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    UNIQUE (media_item_id, username, season, episode) ON CONFLICT REPLACE
) STRICT;

CREATE INDEX idx_plurx_watched_item ON plurx_watched (media_item_id, watched_at DESC);
