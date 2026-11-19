-- +goose Up
ALTER TABLE acquisition_plans ADD COLUMN replacement_origin_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE acquisition_plans ADD COLUMN replacement_origin_generation TEXT NOT NULL DEFAULT '';
CREATE INDEX acquisition_plan_replacement_origin
 ON acquisition_plans(replacement_origin_id,replacement_origin_generation) WHERE replacement_origin_id>0;

-- Older comparisons that were queued solely by the failed-grab worker have
-- no recoverable exact origin. Retire unsent work where that source is proven.
-- A separately queued comparison or manual promotion supplies its own authority.
UPDATE jobs SET state='done',lease_owner='',lease_expires_at=0,finished_at=CAST(strftime('%s','now') AS INTEGER)*1000,
 updated_at=CAST(strftime('%s','now') AS INTEGER)*1000,last_error='replacement origin unavailable after upgrade'
WHERE kind='acquisition.season' AND state IN ('queued','leased') AND json_valid(payload)
 AND json_extract(payload,'$.trigger')='failed_acquisition'
 AND COALESCE(json_extract(payload,'$.manual'),0)=0
 AND COALESCE(json_extract(payload,'$.manualGeneration'),0)=0;

UPDATE downloads SET state='failed',submission_phase='rejected',superseded=1,reserved_episodes='[]',wantables='[]',
 error='replacement origin unavailable after upgrade',updated_at=CAST(strftime('%s','now') AS INTEGER)*1000
WHERE state='planned' AND submission_phase='pending' AND plan_id IN (
 SELECT p.id FROM acquisition_plans p WHERE json_valid(p.decision)
  AND json_extract(p.decision,'$.trigger')='failed_acquisition'
  AND NOT EXISTS (
   SELECT 1 FROM jobs j WHERE j.kind='acquisition.season' AND json_valid(j.payload)
    AND j.dedupe_key='season:'||p.media_item_id||':'||p.copy_id||':'||p.season
    AND json_extract(j.payload,'$.fingerprint')=p.snapshot_fingerprint
    AND (json_extract(j.payload,'$.manual')=1 OR json_extract(j.payload,'$.manualGeneration')>0)
  )
);

-- Release a scope that has no other work. Acknowledged or uncertain
-- children still have their original plan and can reconcile normally.
UPDATE acquisition_plans AS p SET state='completed',revision=revision+1,updated_at=CAST(strftime('%s','now') AS INTEGER)*1000
WHERE p.state IN ('admitted','dispatching','active')
 AND EXISTS (SELECT 1 FROM downloads d WHERE d.plan_id=p.id AND d.error='replacement origin unavailable after upgrade')
 AND NOT EXISTS (
  SELECT 1 FROM downloads d WHERE d.plan_id=p.id AND d.superseded=0
   AND (d.state NOT IN ('failed','imported') OR d.submission_phase IN ('submitting','uncertain'))
 );

-- A season fallback can be scheduled or recovered after the initiating
-- request has returned. Fence its persisted origin at the queue boundary.
-- +goose StatementBegin
CREATE TRIGGER fence_deleted_season_replacement_enqueue BEFORE INSERT ON jobs
WHEN NEW.kind='acquisition.season' AND json_valid(NEW.payload)
 AND json_type(NEW.payload,'$.replacementOrigin')='object'
 AND NOT EXISTS (
   SELECT 1 FROM downloads d
   WHERE d.id=json_extract(NEW.payload,'$.replacementOrigin.downloadId')
     AND d.generation_key=json_extract(NEW.payload,'$.replacementOrigin.generationKey')
     AND NOT EXISTS (
       SELECT 1 FROM download_dispositions x WHERE x.download_id=d.id AND x.generation_key=d.generation_key
     )
 )
BEGIN
 SELECT RAISE(IGNORE);
END;
-- +goose StatementEnd

CREATE INDEX queued_season_replacement_origin
 ON jobs(json_extract(payload,'$.replacementOrigin.downloadId'),json_extract(payload,'$.replacementOrigin.generationKey'))
 WHERE kind='acquisition.season' AND state='queued' AND json_valid(payload);

-- Dispose unsent plan reservations and queued work in the same transaction
-- as the origin's Delete. Claimed children are handled by replacement
-- admission dispositions, and submitted independent children keep custody.
-- +goose StatementBegin
CREATE TRIGGER suppress_deleted_season_replacement AFTER INSERT ON download_dispositions
BEGIN
 UPDATE downloads SET state='failed',submission_phase='rejected',superseded=1,reserved_episodes='[]',wantables='[]',
   error='replacement origin deleted before admission',updated_at=NEW.requested_at
 WHERE state='planned' AND submission_phase='pending' AND plan_id IN (
   SELECT id FROM acquisition_plans WHERE replacement_origin_id>0 AND replacement_origin_id=NEW.download_id
     AND replacement_origin_generation=NEW.generation_key
 );
 UPDATE jobs SET state='done',updated_at=NEW.requested_at,finished_at=NEW.requested_at
 WHERE kind='acquisition.season' AND state='queued' AND json_valid(payload)
   AND json_extract(payload,'$.replacementOrigin.downloadId')=NEW.download_id
   AND json_extract(payload,'$.replacementOrigin.generationKey')=NEW.generation_key;
END;
-- +goose StatementEnd

-- +goose Down
-- An older worker cannot enforce origin authority on live work.
-- +goose StatementBegin
CREATE TEMP TABLE season_replacement_downgrade_guard(n INTEGER CHECK(n=0));
INSERT INTO season_replacement_downgrade_guard
SELECT (SELECT count(*) FROM acquisition_plans
  WHERE replacement_origin_id>0 AND state IN ('admitted','dispatching','active','cancel_requested'))
 + (SELECT count(*) FROM jobs WHERE kind='acquisition.season' AND state IN ('queued','leased','failed')
  AND json_valid(payload) AND json_type(payload,'$.replacementOrigin')='object');
DROP TABLE season_replacement_downgrade_guard;
-- +goose StatementEnd
DROP TRIGGER suppress_deleted_season_replacement;
DROP INDEX queued_season_replacement_origin;
DROP TRIGGER fence_deleted_season_replacement_enqueue;
DROP INDEX acquisition_plan_replacement_origin;
ALTER TABLE acquisition_plans DROP COLUMN replacement_origin_generation;
ALTER TABLE acquisition_plans DROP COLUMN replacement_origin_id;
