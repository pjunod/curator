-- +goose Up
-- Accepting a replacement volume verifies future work only. Existing custody
-- is fenced by acquisition generation AND affected root, including jobs whose
-- downloader has not yet reported its final path.
CREATE TABLE storage_change_review_fences (
 generation_key TEXT NOT NULL,
 root TEXT NOT NULL,
 PRIMARY KEY(generation_key,root)
) STRICT;
CREATE TABLE storage_change_acceptances (
 review_token TEXT PRIMARY KEY,
 root TEXT NOT NULL,
 expected_identity TEXT NOT NULL,
 observed_identity TEXT NOT NULL,
 affected_generations INTEGER NOT NULL,
 accepted_at INTEGER NOT NULL
) STRICT;

-- +goose Down
CREATE TEMP TABLE storage_change_downgrade_guard(ok INTEGER CHECK(ok=1));
INSERT INTO storage_change_downgrade_guard
 SELECT CASE WHEN EXISTS(SELECT 1 FROM storage_change_review_fences) THEN 0 ELSE 1 END;
DROP TABLE storage_change_review_fences;
DROP TABLE storage_change_acceptances;
DROP TABLE storage_change_downgrade_guard;
