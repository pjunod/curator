-- +goose Up
-- A Re-match that ended badly (failed, or complete with files that could not
-- be moved) stays on the item page's status panel for 24 hours or until a
-- person dismisses it; dismissed_at is when (0 = not dismissed).
ALTER TABLE rematch_operations ADD COLUMN dismissed_at INTEGER NOT NULL DEFAULT 0;
