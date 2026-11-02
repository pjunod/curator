-- +goose Up
-- Import permission is separate from cleanup authority. Only historical trace
-- evidence that precedes the ORIGINAL 0040 cutover can admit an old in-flight
-- acquisition. Missing/ambiguous traces remain in explicit review. Seconds at
-- the cutover are deliberately excluded because goose records second precision.
CREATE TABLE legacy_inflight_import_evidence (
 generation_key TEXT PRIMARY KEY,
 cutover_ms INTEGER NOT NULL,
 evidence TEXT NOT NULL
) STRICT;
INSERT INTO legacy_inflight_import_evidence(generation_key,cutover_ms,evidence)
SELECT d.generation_key, c.cutover_ms, d.handoff_log
FROM downloads d
JOIN (SELECT CAST(strftime('%s',tstamp) AS INTEGER)*1000 AS cutover_ms
      FROM goose_db_version WHERE version_id=40 AND is_applied=1
      ORDER BY id DESC LIMIT 1) c
WHERE EXISTS(SELECT 1 FROM legacy_cleanup_review l WHERE l.generation_key=d.generation_key)
 AND json_valid(d.handoff_log)
 AND (SELECT json_extract(j.value,'$.step') FROM json_each(d.handoff_log) j
      WHERE json_extract(j.value,'$.at')>0 AND json_extract(j.value,'$.at')<c.cutover_ms
      ORDER BY CAST(json_extract(j.value,'$.at') AS INTEGER) DESC,j.key DESC LIMIT 1)
     IN ('grabbed','downloading')
 AND NOT EXISTS(SELECT 1 FROM json_each(d.handoff_log) j
      WHERE json_extract(j.value,'$.at')<c.cutover_ms+1000
        AND json_extract(j.value,'$.step') IN
            ('downloaded','awaiting_import','importing','imported','failed','removed','manual_queued','manual_import_queued'));

-- +goose StatementBegin
CREATE TRIGGER legacy_inflight_import_evidence_immutable BEFORE UPDATE ON legacy_inflight_import_evidence
BEGIN SELECT RAISE(ABORT,'legacy in-flight import evidence is immutable'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER legacy_inflight_import_evidence_immutable;
DROP TABLE legacy_inflight_import_evidence;
