package sqlite

import (
	"context"
	"path/filepath"
	"time"
)

// ImportActivityEntry is a small index of retained import work. It includes
// dismissed Activity receipts, but not failures that never reached import.
// Detailed publication manifests are loaded only for the requested page.
type ImportActivityEntry struct {
	ReservationActionable, Superseded, LocalImportFailure, AutomaticImportRetry, ActionableFiles bool
	ID                                                                                           int64
	Protocol, SubmissionPhase, ParkedReason, CancelOutcome                                       string
	State, Title, Kind, Destination                                                              string
	UpdatedAt                                                                                    time.Time
	Review                                                                                       bool
	Cancelled                                                                                    bool
	PendingPublication                                                                           bool
	FailedPublication                                                                            bool
	Unresolved                                                                                   bool
	ImportedPublication                                                                          bool
}

func (d *DB) ImportActivityIndex(ctx context.Context, search string) ([]ImportActivityEntry, error) {
	return d.activityIndex(ctx, search, false)
}

// ActivityIndex includes visible transfers and retained import history once per identity.
func (d *DB) ActivityIndex(ctx context.Context, search string, importsOnly bool) ([]ImportActivityEntry, error) {
	return d.activityIndex(ctx, search, !importsOnly)
}

func (d *DB) activityIndex(ctx context.Context, search string, transfers bool) ([]ImportActivityEntry, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT d.id,d.state,COALESCE(m.title,d.release_title),COALESCE(m.kind,''),
 COALESCE(NULLIF(mc.path,''),m.path,''),
 MAX(d.updated_at,COALESCE((SELECT MAX(p.updated_at) FROM publications p WHERE p.download_id=d.id),d.updated_at)),
 EXISTS(SELECT 1 FROM import_attempt_stages s WHERE s.download_id=d.id AND s.generation_key=d.generation_key AND s.state='held')
 OR EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.download_id=d.id AND a.generation_key=d.generation_key AND (a.outcome='pending' OR a.resolution='retained_for_review'))
 OR EXISTS(SELECT 1 FROM import_placements x WHERE json_extract(x.data,'$.download_id')=d.id AND x.state IN ('prepared','retained')),
 COALESCE(c.outcome IN ('pending','cancelled'),0),
 EXISTS(SELECT 1 FROM publications p WHERE p.download_id=d.id AND p.state IN ('discovered','importing')),
 EXISTS(SELECT 1 FROM publications p WHERE p.download_id=d.id AND (p.state='import_failed' OR (p.kind='completed' AND p.state='removed' AND d.state='failed'))),
 EXISTS(SELECT 1 FROM publications p JOIN publication_files f ON f.publication_id=p.id WHERE p.download_id=d.id AND f.outcome='unresolved'),
 EXISTS(SELECT 1 FROM publications p WHERE p.download_id=d.id AND p.state IN ('imported','notified','removed')),
 d.protocol,d.submission_phase,d.parked_reason,COALESCE(c.outcome,''),
 (d.superseded=0 AND d.cleanup_pending=0 AND d.state NOT IN ('imported','importing') AND (d.submission_phase='uncertain' OR d.parked_at>0)),
 d.superseded,
 (d.state='failed' AND d.runner_outcome!='failed' AND (d.protocol='manual' OR EXISTS(SELECT 1 FROM json_each(d.handoff_log) h WHERE json_extract(h.value,'$.step') IN ('downloaded','importing','manual_import_queued')))),
 (d.state='failed' AND d.delivery_expected=0 AND instr(d.error,'import stopped')>0 AND (d.import_path!='' OR d.save_path!='')),
 EXISTS(SELECT 1 FROM publications p JOIN publication_files f ON f.publication_id=p.id WHERE p.download_id=d.id AND f.outcome='unresolved' AND f.reason!='kept by an operator in Resolve' AND (d.protocol='manual' OR f.reason='not selected for import; place or discard it in Resolve'))
 FROM downloads d LEFT JOIN media_items m ON m.id=d.media_item_id
 LEFT JOIN media_copies mc ON mc.id=d.copy_id AND mc.media_item_id=d.media_item_id
 LEFT JOIN import_attempt_controls c ON c.download_id=d.id AND c.generation_key=d.generation_key
 WHERE ((? AND d.activity_dismissed=0) OR d.protocol='manual' OR d.state IN ('downloaded','awaiting_import','importing','imported')
 OR d.import_path!='' OR EXISTS(SELECT 1 FROM publications p WHERE p.download_id=d.id)
 OR c.download_id IS NOT NULL)
 AND (?='' OR instr(lower(d.release_title),lower(?))>0 OR instr(lower(COALESCE(m.title,'')),lower(?))>0)
 UNION ALL
 SELECT -p.id,p.state,p.path,'','',p.updated_at,p.state NOT IN ('imported','notified','removed'),0,0,
 p.state='import_failed',
 EXISTS(SELECT 1 FROM publication_files f WHERE f.publication_id=p.id AND f.outcome='unresolved'),1,'historical','','','',0,0,0,0,0
 FROM publications p WHERE p.download_id IS NULL
 AND (p.state IN ('imported','notified') OR EXISTS(SELECT 1 FROM publication_files f WHERE f.publication_id=p.id AND f.outcome='placed'))
 AND (?='' OR instr(lower(p.path),lower(?))>0)
 ORDER BY 6 DESC,1 DESC`, transfers, search, search, search, search, search)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ImportActivityEntry{}
	for rows.Next() {
		var e ImportActivityEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.State, &e.Title, &e.Kind, &e.Destination, &at, &e.Review, &e.Cancelled, &e.PendingPublication, &e.FailedPublication, &e.Unresolved, &e.ImportedPublication, &e.Protocol, &e.SubmissionPhase, &e.ParkedReason, &e.CancelOutcome, &e.ReservationActionable, &e.Superseded, &e.LocalImportFailure, &e.AutomaticImportRetry, &e.ActionableFiles); err != nil {
			return nil, err
		}
		e.UpdatedAt = time.UnixMilli(at)
		if e.ID < 0 {
			e.Title = filepath.Base(e.Title)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
