-- +goose Up
-- Pipeline contract v2, M6b: nothing writes the v1_record manifest source any
-- more. Curator reads a publication's manifest from its own
-- .runner-manifest.json; a publication that already carries a stored v1
-- record body keeps it (publicationManifest falls back to it) and drains
-- through its import and per-file cleanup like any other. Nothing is refused:
-- v0.45.x stamped v1_record on nearly every new publication.
--
-- Only a removed publication's record is normalised (its files are gone; the
-- stored manifest is of no further use): the source becomes 'none'.
--
-- The manifest_source CHECK is left as it is: narrowing it would rebuild
-- publications, and notifier_deliveries and publication_files cascade from it.
-- Nothing writes 'v1_record' any more.
UPDATE publications
   SET manifest_source = 'none', manifest_json = '{}'
 WHERE manifest_source = 'v1_record' AND state = 'removed';

-- +goose Down
-- The normalised rows were removed publications with no files left; there is
-- nothing to restore.
SELECT 1;
