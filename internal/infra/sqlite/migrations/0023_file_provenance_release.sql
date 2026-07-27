-- +goose Up
-- Remember which release put a file here.
--
-- Until now a file on disk was an orphan: monarr knew its path, its size and
-- (since 0018) what it measures as, but nothing tied it back to the release
-- that produced it. That gap is only visible when something goes wrong. A user
-- looking at a bad file wants to say "this one is garbage, never take it
-- again" — and blocklisting needs a release title and an indexer, which lived
-- only in the downloads row and only until the queue moved on.
--
-- Two columns, both nullable-by-default-empty, because most rows will never
-- have them: a file adopted from an existing library was not grabbed by monarr
-- and honestly has no source release. Empty means "we do not know", and the
-- UI offers deletion alone in that case rather than pretending otherwise.
ALTER TABLE media_files ADD COLUMN source_release TEXT NOT NULL DEFAULT '';
ALTER TABLE media_files ADD COLUMN source_indexer TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE media_files DROP COLUMN source_release;
ALTER TABLE media_files DROP COLUMN source_indexer;
