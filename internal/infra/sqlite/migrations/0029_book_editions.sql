-- +goose Up
-- A book work can own an ebook and an audiobook at the same time (ADR 0018).
--
-- The acquisition pipeline already gives every additional target its own
-- copy_id, profile, files, downloads, and wanted lifecycle. What it lacked
-- was an explicit statement of WHAT a book target is. Inferring that from a
-- profile made changing profiles silently change the medium and made the two
-- editions mutually exclusive.
--
-- Keep the existing primary/additional target storage for compatibility, but
-- persist the book type independently on both rows. Existing books are
-- backfilled once from their current profile; after this migration profiles
-- are policy within an edition, never the edition's identity.

ALTER TABLE media_items
    ADD COLUMN book_type TEXT NOT NULL DEFAULT ''
    CHECK (book_type IN ('', 'ebook', 'audiobook'));

ALTER TABLE media_copies
    ADD COLUMN book_type TEXT NOT NULL DEFAULT ''
    CHECK (book_type IN ('', 'ebook', 'audiobook'));

UPDATE media_items
SET book_type = CASE
    WHEN EXISTS (
        SELECT 1
        FROM quality_profiles qp
        WHERE qp.id = media_items.quality_profile_id
          AND json_extract(qp.definition, '$.target.source') IN
              ('mp3', 'wma', 'aac', 'ogg', 'opus', 'm4a', 'm4b', 'flac', 'wav')
    ) THEN 'audiobook'
    ELSE 'ebook'
END
WHERE kind = 'book';
