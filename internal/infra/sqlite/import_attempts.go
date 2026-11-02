package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

var (
	ErrImportAttemptChanged     = errors.New("import attempt changed")
	ErrImportCancelled          = errors.New("import is suppressed by durable cancellation")
	ErrImportCompleted          = errors.New("import already completed")
	ErrImportPublicationPending = errors.New("publication requires reconciliation before cancellation or retry")
	ErrImportPlanChanged        = errors.New("import selection changed; explicit retry required")
	ErrImportReviewRequired     = errors.New("retained publication requires explicit Library review before this target can be published")
)

type ImportAttemptKey struct {
	DownloadID    int64  `json:"downloadId"`
	GenerationKey string `json:"generationKey"`
	Epoch         int64  `json:"epoch"`
}

type ImportAttempt struct {
	ImportAttemptKey
	Outcome              string `json:"outcome"`
	PlanSealed           bool   `json:"planSealed"`
	PendingPublications  int    `json:"pendingPublications"`
	RetainedPublications int    `json:"retainedPublications"`
	HeldStages           int    `json:"heldStages"`
}

type ImportAttemptFile struct{ Source, Kind, Outcome, PlacementID string }

func readImportAttempt(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64, generation string) (ImportAttempt, error) {
	var a ImportAttempt
	err := q.QueryRowContext(ctx, `SELECT download_id,generation_key,attempt_epoch,outcome,plan_sealed,
 (SELECT count(*) FROM import_publication_authorizations p WHERE p.download_id=c.download_id AND p.generation_key=c.generation_key AND p.outcome='pending'),
 (SELECT count(*) FROM import_publication_authorizations p WHERE p.download_id=c.download_id AND p.generation_key=c.generation_key AND p.resolution='retained_for_review'),
 (SELECT count(*) FROM import_attempt_stages s WHERE s.download_id=c.download_id AND s.generation_key=c.generation_key AND s.state='held')+
 (SELECT count(*) FROM import_placements p WHERE json_extract(p.data,'$.download_id')=c.download_id AND json_extract(p.data,'$.source_generation')=c.generation_key AND p.state IN ('prepared','retained'))
 FROM import_attempt_controls c WHERE download_id=? AND generation_key=?`, id, generation).Scan(&a.DownloadID, &a.GenerationKey, &a.Epoch, &a.Outcome, &a.PlanSealed, &a.PendingPublications, &a.RetainedPublications, &a.HeldStages)
	return a, err
}

func ensureImportAttempt(ctx context.Context, tx *sql.Tx, id int64, generation string) (ImportAttempt, error) {
	var current, state string
	if err := tx.QueryRowContext(ctx, `SELECT generation_key,state FROM downloads WHERE id=?`, id).Scan(&current, &state); err != nil {
		return ImportAttempt{}, wrapNotFound(err)
	}
	if current != generation {
		return ImportAttempt{}, ErrImportAttemptChanged
	}
	outcome := ""
	var finalized any
	if state == "imported" {
		outcome = "completed"
		finalized = time.Now().UnixMilli()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO import_attempt_controls(download_id,generation_key,outcome,finalized_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, id, generation, outcome, finalized); err != nil {
		return ImportAttempt{}, err
	}
	return readImportAttempt(ctx, tx, id, generation)
}

func guardImportAttempt(ctx context.Context, tx *sql.Tx, key ImportAttemptKey) (ImportAttempt, error) {
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT generation_key FROM downloads WHERE id=?`, key.DownloadID).Scan(&current); err != nil {
		return ImportAttempt{}, wrapNotFound(err)
	}
	if current != key.GenerationKey {
		return ImportAttempt{}, ErrImportAttemptChanged
	}
	a, err := readImportAttempt(ctx, tx, key.DownloadID, key.GenerationKey)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrImportAttemptChanged
	}
	if err == nil && a.Epoch != key.Epoch {
		err = ErrImportAttemptChanged
	}
	return a, err
}

func (d *DB) EnsureImportAttempt(ctx context.Context, id int64, generation string) (ImportAttempt, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return ImportAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := ensureImportAttempt(ctx, tx, id, generation)
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (d *DB) GetImportAttempt(ctx context.Context, id int64, generation string) (ImportAttempt, error) {
	return readImportAttempt(ctx, d.R, id, generation)
}

func (d *DB) BeginImportRetry(ctx context.Context, id int64, generation string) (ImportAttempt, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return ImportAttempt{}, err
	}
	defer tx.Rollback()
	a, err := beginImportRetry(ctx, tx, id, generation, true)
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func beginImportRetry(ctx context.Context, tx *sql.Tx, id int64, generation string, recordStep bool) (ImportAttempt, error) {
	a, err := ensureImportAttempt(ctx, tx, id, generation)
	if err != nil {
		return a, err
	}
	if a.PendingPublications > 0 {
		return a, ErrImportPublicationPending
	}
	if a.RetainedPublications > 0 {
		return a, ErrImportReviewRequired
	}
	if a.Outcome == "pending" {
		return a, ErrImportCancelled
	}

	if a.Epoch == int64(^uint64(0)>>1) {
		return a, fmt.Errorf("import epoch exhausted")
	}
	_, err = tx.ExecContext(ctx, `UPDATE import_attempt_controls SET attempt_epoch=attempt_epoch+1,outcome='',requested_at=NULL,finalized_at=NULL,plan_sealed=0 WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, id, generation, a.Epoch)
	if err != nil {
		return a, err
	}
	a.Epoch++
	a.Outcome = ""
	a.PlanSealed = false
	if recordStep {
		if err = writeImportState(ctx, tx, a.ImportAttemptKey, "downloaded", "import_queued", "waiting for an import worker"); err != nil {
			return a, err
		}
	}
	return a, nil
}

// A complete plan includes potential sidecars, so media completion cannot race
// a later caption publication. The media importer may validate a subset of an
// already sealed delivery plan, but can never extend it after authorization.
func (d *DB) SealImportAttempt(ctx context.Context, key ImportAttemptKey, files []ImportAttemptFile, subset bool) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return err
	}
	if a.Outcome != "" {
		return ErrImportCancelled
	}
	seen := map[string]string{}
	for _, f := range files {
		if f.Source == "" || (f.Kind != "media" && f.Kind != "sidecar") {
			return ErrImportPlanChanged
		}
		if _, ok := seen[f.Source]; ok {
			return ErrImportPlanChanged
		}
		seen[f.Source] = f.Kind
	}
	if a.PlanSealed {
		rows, e := tx.QueryContext(ctx, `SELECT source,kind FROM import_attempt_files WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, key.DownloadID, key.GenerationKey, key.Epoch)
		if e != nil {
			return e
		}
		existing := map[string]string{}
		for rows.Next() {
			var source, kind string
			if e = rows.Scan(&source, &kind); e != nil {
				_ = rows.Close()
				return e
			}
			existing[source] = kind
		}
		e = rows.Err()
		_ = rows.Close()
		if e != nil {
			return e
		}
		if !subset && len(existing) != len(seen) {
			return ErrImportPlanChanged
		}
		for source, kind := range seen {
			if existing[source] != kind {
				return ErrImportPlanChanged
			}
		}
		return tx.Commit()
	}
	for source, kind := range seen {
		if _, err = tx.ExecContext(ctx, `INSERT INTO import_attempt_files(download_id,generation_key,attempt_epoch,source,kind) VALUES(?,?,?,?,?)`, key.DownloadID, key.GenerationKey, key.Epoch, source, kind); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE import_attempt_controls SET plan_sealed=1 WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, key.DownloadID, key.GenerationKey, key.Epoch)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func resolveImportFile(ctx context.Context, tx *sql.Tx, key ImportAttemptKey, source, outcome, placementID string) error {
	if outcome != "skipped" && outcome != "committed" {
		return ErrImportPlanChanged
	}
	result, err := tx.ExecContext(ctx, `UPDATE import_attempt_files SET outcome=?,placement_id=? WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND source=? AND (outcome='pending' OR outcome=?)`, outcome, placementID, key.DownloadID, key.GenerationKey, key.Epoch, source, outcome)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrImportPlanChanged
	}
	return nil
}

func (d *DB) ResolveImportFile(ctx context.Context, key ImportAttemptKey, source, outcome, placementID string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return err
	}
	if a.Outcome == "cancelled" || a.Outcome == "completed" {
		return ErrImportCancelled
	}
	if err = resolveImportFile(ctx, tx, key, source, outcome, placementID); err != nil {
		return err
	}
	return tx.Commit()
}

func importPlanComplete(ctx context.Context, tx *sql.Tx, a ImportAttempt) (bool, error) {
	if !a.PlanSealed {
		return false, nil
	}
	var pending, committed int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE outcome='pending'),count(*) FILTER(WHERE outcome='committed') FROM import_attempt_files WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, a.DownloadID, a.GenerationKey, a.Epoch).Scan(&pending, &committed)
	return pending == 0 && committed > 0, err
}

// writeImportState reads handoff JSON on W inside the same transaction as the
// control transition; it never overwrites it with a worker's stale snapshot.
func writeImportState(ctx context.Context, tx *sql.Tx, key ImportAttemptKey, state, step, detail string) error {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT handoff_log FROM downloads WHERE id=? AND generation_key=?`, key.DownloadID, key.GenerationKey).Scan(&raw); err != nil {
		return err
	}
	var handoff []HandoffEntry
	if err := json.Unmarshal([]byte(raw), &handoff); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if len(handoff) == 0 || handoff[len(handoff)-1].Step != step {
		handoff = append(handoff, HandoffEntry{Step: step, Detail: detail, At: now})
	}
	data, err := json.Marshal(handoff)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE downloads SET state=?,progress=CASE WHEN ?='imported' THEN 1 ELSE progress END,error='',handoff_log=?,updated_at=? WHERE id=? AND generation_key=?`, state, state, string(data), now, key.DownloadID, key.GenerationKey)
	return err
}

func completeImportAttempt(ctx context.Context, tx *sql.Tx, a ImportAttempt, details ...string) error {
	if a.PendingPublications > 0 {
		return ErrImportPublicationPending
	}
	if a.Outcome == "cancelled" {
		return ErrImportCancelled
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_attempt_controls SET outcome='completed',finalized_at=? WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, time.Now().UnixMilli(), a.DownloadID, a.GenerationKey, a.Epoch); err != nil {
		return err
	}
	detail := "selected import work completed; committed library files retained"
	if len(details) > 0 {
		detail = details[0]
	}
	return writeImportState(ctx, tx, a.ImportAttemptKey, "imported", "imported", detail)
}

func (d *DB) RequestImportCancellation(ctx context.Context, key ImportAttemptKey) (ImportAttempt, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return ImportAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return a, err
	}
	if a.Outcome == "completed" || a.Outcome == "cancelled" {
		return a, tx.Commit()
	}
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM downloads WHERE id=?`, key.DownloadID).Scan(&state); err != nil {
		return a, err
	}
	complete, e := importPlanComplete(ctx, tx, a)
	if e != nil {
		return a, e
	}
	if a.PendingPublications == 0 && (state == "imported" || complete) {
		if err = completeImportAttempt(ctx, tx, a); err != nil {
			return a, err
		}
		a.Outcome = "completed"
		return a, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE import_attempt_controls SET outcome='pending',requested_at=COALESCE(requested_at,?) WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, time.Now().UnixMilli(), key.DownloadID, key.GenerationKey, key.Epoch)
	if err != nil {
		return a, err
	}
	a.Outcome = "pending"
	return a, tx.Commit()
}

func (d *DB) FinalizeImportCancellation(ctx context.Context, key ImportAttemptKey) (ImportAttempt, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return ImportAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return a, err
	}
	if a.Outcome != "pending" {
		return a, tx.Commit()
	}
	if a.PendingPublications > 0 {
		return a, ErrImportPublicationPending
	}
	complete, e := importPlanComplete(ctx, tx, a)
	if e != nil {
		return a, e
	}
	if complete {
		if err = completeImportAttempt(ctx, tx, a); err != nil {
			return a, err
		}
		a.Outcome = "completed"
		return a, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE import_attempt_controls SET outcome='cancelled',finalized_at=? WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, time.Now().UnixMilli(), key.DownloadID, key.GenerationKey, key.Epoch)
	if err != nil {
		return a, err
	}
	if err = writeImportState(ctx, tx, key, "downloaded", "import_cancelled", "import cancelled; source, committed files and uncertain bytes kept"); err != nil {
		return a, err
	}
	a.Outcome = "cancelled"
	return a, tx.Commit()
}

// CompleteImportAttempt is called only by the worker that finished its selected
// plan. Deterministic skips preserve existing import policy; unresolved files or
// authorizations never become completed merely because a context was canceled.
func (d *DB) CompleteImportAttempt(ctx context.Context, key ImportAttemptKey, details ...string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return err
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM import_attempt_files WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND outcome='pending'`, key.DownloadID, key.GenerationKey, key.Epoch).Scan(&pending); err != nil {
		return err
	}
	if !a.PlanSealed || pending > 0 {
		return ErrImportPlanChanged
	}
	if err = completeImportAttempt(ctx, tx, a, details...); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) PendingImportCancellations(ctx context.Context) ([]ImportAttemptKey, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT c.download_id,c.generation_key,c.attempt_epoch FROM import_attempt_controls c JOIN downloads d ON d.id=c.download_id AND d.generation_key=c.generation_key WHERE c.outcome='pending' ORDER BY c.requested_at LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []ImportAttemptKey
	for rows.Next() {
		var key ImportAttemptKey
		if err = rows.Scan(&key.DownloadID, &key.GenerationKey, &key.Epoch); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (d *DB) ImportAttemptFiles(ctx context.Context, key ImportAttemptKey) ([]ImportAttemptFile, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT source,kind,outcome,COALESCE(placement_id,'') FROM import_attempt_files WHERE download_id=? AND generation_key=? AND attempt_epoch=?`, key.DownloadID, key.GenerationKey, key.Epoch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var files []ImportAttemptFile
	for rows.Next() {
		var f ImportAttemptFile
		if err = rows.Scan(&f.Source, &f.Kind, &f.Outcome, &f.PlacementID); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Source < files[j].Source })
	return files, rows.Err()
}
