-- +goose Up
CREATE TABLE library_review_flags (
    id INTEGER PRIMARY KEY,
    media_item_id INTEGER NOT NULL,
    media_file_id INTEGER NOT NULL,
    file_generation TEXT NOT NULL,
    reason_code TEXT NOT NULL,
    reason TEXT NOT NULL,
    evidence TEXT NOT NULL DEFAULT '{}',
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','resolved')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(media_file_id,file_generation)
) STRICT;
CREATE INDEX library_review_flags_open ON library_review_flags(state,media_item_id);

-- +goose Down
CREATE TEMP TABLE library_review_flags_downgrade_guard (ok INTEGER NOT NULL CHECK (ok=1));
INSERT INTO library_review_flags_downgrade_guard
SELECT CASE WHEN EXISTS (SELECT 1 FROM library_review_flags) THEN 0 ELSE 1 END;
DROP TABLE library_review_flags_downgrade_guard;
DROP INDEX library_review_flags_open;
DROP TABLE library_review_flags;
