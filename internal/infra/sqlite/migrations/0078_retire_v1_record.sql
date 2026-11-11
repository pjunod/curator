-- +goose Up
-- Pipeline contract v2, M6b: the v1_record manifest source is retired. Curator
-- reads a publication's manifest from its own .runner-manifest.json only.
--
-- Migrate.go refuses to run this file while a publication that is not yet
-- removed still has manifest_source = 'v1_record' (it would be stranded with
-- no manifest to import or clean from), naming how many and which. By the time
-- it runs, every such row is 'removed', its files gone: the stored manifest is
-- of no further use, so the source is normalised to 'none'.
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
