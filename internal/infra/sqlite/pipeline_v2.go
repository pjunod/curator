package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pjunod/monarr/internal/domain/backoff"
	"path/filepath"
	"strings"
	"time"

	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
)

// Pipeline contract v2, M2: the reducer's persistence. Every write that has
// to land together with a terminal lives here, in one transaction, because
// the writer pool is a single connection: an app-level transaction that then
// called AddHistory or AddBlocklist (each taking its own connection) would
// deadlock on it.

// Deadlines (contract section 3.5).
const (
	DeadlineSubmitting  = 10 * time.Minute
	DeadlineDownloading = 30 * time.Minute
	DeadlineTorrent     = 24 * time.Hour
	DeadlineHeldGrace   = 5 * time.Minute
	DeadlineHeldLegacy  = 24 * time.Hour
	DeadlinePublication = 6 * time.Hour
)

// PipelineCutoverSetting records the first M2 start; the historical cohort
// is marked exactly once, keyed on it.
const PipelineCutoverSetting = "pipeline_v2_cutover_at"

// DownloadByTransfer finds the newest row of one client carrying an exact
// transfer token, whatever its state. The reducer, not the lookup, decides
// what a fact may still do to it.
func (d *DB) DownloadByTransfer(ctx context.Context, clientID int64, transfer string) (Download, bool, error) {
	if transfer == "" {
		return Download{}, false, nil
	}
	var id int64
	err := d.R.QueryRowContext(ctx, `SELECT id FROM downloads WHERE client_id=? AND transfer=? ORDER BY id DESC LIMIT 1`, clientID, transfer).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Download{}, false, nil
	}
	if err != nil {
		return Download{}, false, err
	}
	dl, err := d.GetDownload(ctx, id)
	return dl, err == nil, err
}

// DownloadByTransferAnyClient finds the newest row carrying an exact
// transfer token, on any client: a publication directory names its transfer
// but not the client that submitted it.
func (d *DB) DownloadByTransferAnyClient(ctx context.Context, transfer string) (Download, bool, error) {
	if transfer == "" {
		return Download{}, false, nil
	}
	var id int64
	err := d.R.QueryRowContext(ctx, `SELECT id FROM downloads WHERE transfer=? ORDER BY id DESC LIMIT 1`, transfer).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Download{}, false, nil
	}
	if err != nil {
		return Download{}, false, err
	}
	dl, err := d.GetDownload(ctx, id)
	return dl, err == nil, err
}

// DownloadByHandle is the legacy fallback for an event that carried no
// transfer token: the newest row of the client with that handle whose
// terminal is still open (or provisional).
func (d *DB) DownloadByHandle(ctx context.Context, clientID int64, handle string) (Download, bool, error) {
	if handle == "" {
		return Download{}, false, nil
	}
	var id int64
	err := d.R.QueryRowContext(ctx, `SELECT id FROM downloads WHERE client_id=? AND handle=? AND (runner_outcome='' OR provisional=1) ORDER BY id DESC LIMIT 1`, clientID, handle).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Download{}, false, nil
	}
	if err != nil {
		return Download{}, false, err
	}
	dl, err := d.GetDownload(ctx, id)
	return dl, err == nil, err
}

// OutstandingRunnerDownloads lists a client's rows whose terminal is still
// open or provisional: what a continuity loss must reconcile one by one.
func (d *DB) OutstandingRunnerDownloads(ctx context.Context, clientID int64) ([]Download, error) {
	return d.downloadsWhere(ctx, `client_id=? AND transfer!='' AND (runner_outcome='' OR provisional=1) AND state NOT IN ('planned')`, clientID)
}

func (d *DB) downloadsWhere(ctx context.Context, where string, args ...any) ([]Download, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM downloads WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	// Close does not report an iteration failure; without Err a read that
	// fails mid-scan would return a truncated list as if it were complete.
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// One batched read per chunk, not a GetDownload per row.
	out := make([]Download, 0, len(ids))
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		rows, err := d.Read.ListDownloadsByIDs(ctx, chunk)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, downloadFromRow(r))
		}
	}
	return out, nil
}

// HistoryWrite is one history_events row written inside a composite write.
type HistoryWrite struct {
	Type         string
	MediaItemID  int64
	ReleaseTitle string
	Data         any
}

// BlocklistWrite blocklists the release a terminal blamed.
type BlocklistWrite struct {
	MediaItemID     int64
	SourceIndexerID int64
	SourceGUID      string
	Title, Indexer  string
	Reason          string
}

// PublicationWrite is the publications row a completed terminal or a salvage
// fact implies.
type PublicationWrite struct {
	DownloadID   int64
	Generation   string
	Kind         string // completed | salvage
	Path         string // Curator's view (mapped)
	RemotePath   string // Runner's view
	ManifestJSON string // '' when no manifest body is known yet
	Source       string // file | v1_record | none
	CrossVolume  bool
	Cohort       string
}

// ReplacementGrabWrite is the work a failed terminal with a re-grab due
// implies; the replacement grab worker executes it.
type ReplacementGrabWrite struct {
	WantableIDs []string
}

// TimingStart opens a pipeline_timings row (first start wins).
type TimingStart struct {
	Kind, Transfer string
	DownloadID     int64
}

// RunnerTerminalWrite is everything one terminal records, atomically.
type RunnerTerminalWrite struct {
	DownloadID    int64
	Transfer      string
	Outcome       string
	Reason        string
	FetchCause    string
	BlamesRelease bool
	Salvage       string
	FetchedJSON   string
	HistorySeq    int64
	Provisional   bool
	State         string
	Error         string
	Progress      float64
	SavePath      string
	ImportPath    string
	DeadlineAt    time.Time
	Handoff       *HandoffEntry
	// ReplanPlanID marks a season plan for re-planning in the same write.
	ReplanPlanID int64
	// CancelPlanID asks a plan to stop: the operator removed its download.
	CancelPlanID    int64
	Blocklist       *BlocklistWrite
	Publication     *PublicationWrite
	ReplacementGrab *ReplacementGrabWrite
	History         []HistoryWrite
	TimingStarts    []TimingStart
	// IndexerOutage records fetch{indexer_unavailable}: the source indexer
	// is deferred until the later of its own retry_at and this time, and the
	// row carries that as provider_retry_after (IMPL section 5.6 item 3).
	IndexerOutage *IndexerOutageWrite
}

// IndexerOutageWrite defers an indexer Runner could not fetch from.
type IndexerOutageWrite struct {
	IndexerID int64 // 0: the row names no indexer; only the row is marked
	Until     time.Time
}

// RecordRunnerTerminal applies the once-guard (an open or provisional
// terminal for exactly this row and token) and, only when it won, every
// write the terminal implies, in one transaction. won=false is a replay.
func (d *DB) RecordRunnerTerminal(ctx context.Context, w RunnerTerminalWrite) (bool, error) {
	if w.DownloadID <= 0 || w.Outcome == "" || w.State == "" {
		return false, fmt.Errorf("runner terminal requires a download, an outcome and a state")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	provisional := 0
	if w.Provisional {
		provisional = 1
	}
	blames := 0
	if w.BlamesRelease {
		blames = 1
	}
	fetched := w.FetchedJSON
	if fetched == "" {
		fetched = "{}"
	}
	entry := "null"
	if w.Handoff != nil {
		raw, _ := json.Marshal(w.Handoff)
		entry = string(raw)
	}
	// A provisional terminal may not replace a runner-authored one; a
	// runner-authored one replaces a provisional one (contract section 3.2).
	guard := `(runner_outcome='' OR provisional=1)`
	res, err := tx.ExecContext(ctx, `UPDATE downloads SET runner_outcome=?, runner_outcome_at=?, runner_outcome_seq=?,
		runner_outcome_reason=?, runner_fetch_cause=?, runner_blames_release=?, runner_salvage=?, runner_fetched_json=?,
		provisional=?, state=?, state_entered_at=?, deadline_at=?, error=?, progress=?,
		save_path=CASE WHEN ?!='' THEN ? ELSE save_path END,
		import_path=CASE WHEN ?!='' THEN ? ELSE import_path END,
		handoff_log=CASE WHEN ?='null' THEN handoff_log ELSE json_insert(handoff_log,'$[#]',json(?)) END,
		parked_reason='', parked_at=0, superseded=0,
		updated_at=?
		WHERE id=? AND transfer=? AND `+guard,
		w.Outcome, now, w.HistorySeq, w.Reason, w.FetchCause, blames, w.Salvage, fetched,
		provisional, w.State, now, unixMilliOrZero(w.DeadlineAt), w.Error, w.Progress,
		w.SavePath, w.SavePath, w.ImportPath, w.ImportPath, entry, entry, now, w.DownloadID, w.Transfer)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	q := d.Write.WithTx(tx)
	if o := w.IndexerOutage; o != nil {
		until := o.Until.UnixMilli()
		if o.IndexerID > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE indexers SET retry_at=MAX(retry_at,?) WHERE id=?`, until, o.IndexerID); err != nil {
				return false, err
			}
			if err := tx.QueryRowContext(ctx, `SELECT MAX(retry_at,?) FROM indexers WHERE id=?`, until, o.IndexerID).Scan(&until); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE downloads SET provider_retry_after=MAX(provider_retry_after,?) WHERE id=?`, until, w.DownloadID); err != nil {
			return false, err
		}
	}
	if w.ReplanPlanID != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE acquisition_plans SET replan_pending=1 WHERE id=?`, w.ReplanPlanID); err != nil {
			return false, err
		}
	}
	if w.CancelPlanID != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE acquisition_plans SET state='cancel_requested' WHERE id=? AND state IN ('admitted','dispatching','active')`, w.CancelPlanID); err != nil {
			return false, err
		}
	}
	if b := w.Blocklist; b != nil {
		if b.SourceGUID != "" && b.SourceIndexerID > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO release_blacklist_identity(media_item_id,guid,indexer_id,reason,added_at) VALUES(?,?,?,?,?) ON CONFLICT(media_item_id,indexer_id,guid) DO UPDATE SET reason=excluded.reason`,
				b.MediaItemID, b.SourceGUID, b.SourceIndexerID, b.Reason, now); err != nil {
				return false, err
			}
		} else if _, err := q.InsertBlocklist(ctx, sqlitegen.InsertBlocklistParams{MediaItemID: b.MediaItemID, ReleaseTitle: b.Title, Indexer: b.Indexer, Reason: b.Reason, CreatedAt: now}); err != nil {
			return false, err
		}
	}
	if p := w.Publication; p != nil {
		var claimed *PublicationPathClaimedError
		if _, err := insertPublicationTx(ctx, tx, *p, now); errors.As(err, &claimed) {
			// Another download's publication owns the directory: this row's
			// import fails here, visibly, instead of waiting at downloaded
			// with no publication and no deadline.
			reason := "publication import_failed: " + claimed.Error()
			fail, _ := json.Marshal(HandoffEntry{Step: "failed", At: now, Detail: reason})
			if _, err := tx.ExecContext(ctx, `UPDATE downloads SET state='failed', error=?, deadline_at=0, state_entered_at=?,
				handoff_log=json_insert(handoff_log,'$[#]',json(?)) WHERE id=?`, reason, now, string(fail), w.DownloadID); err != nil {
				return false, err
			}
		} else if err != nil {
			return false, err
		}
	}
	if g := w.ReplacementGrab; g != nil {
		raw, _ := json.Marshal(g.WantableIDs)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replacement_grabs (download_id, wantable_ids, created_at, next_at) VALUES (?,?,?,?)`,
			w.DownloadID, string(raw), now, now); err != nil {
			return false, err
		}
	}
	for _, h := range w.History {
		if err := insertHistoryTx(ctx, q, h, now); err != nil {
			return false, err
		}
	}
	for _, t := range w.TimingStarts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pipeline_timings (transfer, download_id, kind, started_at) VALUES (?,?,?,?) ON CONFLICT (kind, transfer) DO NOTHING`,
			t.Transfer, t.DownloadID, t.Kind, now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func insertHistoryTx(ctx context.Context, q *sqlitegen.Queries, h HistoryWrite, now int64) error {
	raw, _ := json.Marshal(h.Data)
	if h.Data == nil {
		raw = []byte("{}")
	}
	return q.InsertHistory(ctx, sqlitegen.InsertHistoryParams{Ts: now, Type: h.Type, MediaItemID: h.MediaItemID, ReleaseTitle: h.ReleaseTitle, Data: string(raw)})
}

func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// ReopenProvisional reverts a Curator-authored terminal so a Runner fact can
// be applied as if it had not happened. Only provisional rows re-open; the
// immutability trigger refuses anything else.
func (d *DB) ReopenProvisional(ctx context.Context, id int64, transfer, state string, deadline time.Time, entry HandoffEntry) (bool, error) {
	raw, _ := json.Marshal(entry)
	now := time.Now().UnixMilli()
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET runner_outcome='', runner_outcome_reason='', runner_outcome_at=0, runner_outcome_seq=0,
		provisional=0, state=?, state_entered_at=?, deadline_at=?, error='', parked_reason='', parked_at=0, superseded=0,
		handoff_log=json_insert(handoff_log,'$[#]',json(?)), updated_at=?
		WHERE id=? AND transfer=? AND provisional=1`, state, now, unixMilliOrZero(deadline), string(raw), now, id, transfer)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RunnerStatusWrite is one non-terminal observation.
type RunnerStatusWrite struct {
	DownloadID int64
	Transfer   string
	State      string // downloading | held
	Progress   float64
	Error      string
	Control    string // newest control, for display only ('' keeps the stored one)
	Stage      string // post-processing stage ('' keeps the stored one)
	DeadlineAt time.Time
	Handoff    *HandoffEntry
}

// UpdateRunnerStatus applies a status fact to a row whose terminal is still
// open. A status never moves a terminal row (contract section 3.2), and the
// guard is in SQL, not in the caller.
func (d *DB) UpdateRunnerStatus(ctx context.Context, w RunnerStatusWrite) (bool, error) {
	entry := "null"
	if w.Handoff != nil {
		raw, _ := json.Marshal(w.Handoff)
		entry = string(raw)
	}
	now := time.Now().UnixMilli()
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET
		state_entered_at=CASE WHEN state!=? THEN ? ELSE state_entered_at END,
		state=?, progress=?, error=?,
		runner_control=CASE WHEN ?!='' THEN ? ELSE runner_control END,
		runner_stage=CASE WHEN ?!='' THEN ? ELSE runner_stage END,
		deadline_at=?,
		handoff_log=CASE WHEN ?='null' THEN handoff_log ELSE json_insert(handoff_log,'$[#]',json(?)) END,
		updated_at=?
		WHERE id=? AND transfer=? AND runner_outcome='' AND state IN ('grabbed','submitting','downloading','held')`,
		w.State, now, w.State, w.Progress, w.Error, w.Control, w.Control, w.Stage, w.Stage, unixMilliOrZero(w.DeadlineAt), entry, entry, now, w.DownloadID, w.Transfer)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkHeldStale says a held row's local deadline passed with no word from
// Runner. The row stays held: Curator never authors hold_expired.
func (d *DB) MarkHeldStale(ctx context.Context, id int64, message string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET error=?, updated_at=? WHERE id=? AND state='held' AND runner_outcome=''`, message, time.Now().UnixMilli(), id)
	return err
}

// ---- publications ---------------------------------------------------------

// Publication is one Runner generation Curator imports, notifies and cleans
// up on its own lifecycle (contract section 3.4).
type Publication struct {
	ID                int64
	DownloadID        int64 // 0 for a historical directory with no row
	Generation        string
	Kind              string
	Path              string
	RemotePath        string
	ManifestJSON      string
	ManifestSource    string
	CrossVolume       bool
	State             string
	Cohort            string
	CleanupApprovedAt time.Time
	CleanupPausedAt   time.Time
	Error             string
	DiscoveredAt      time.Time
	DeadlineAt        time.Time
	UpdatedAt         time.Time
	// LastStop is why the last attempt stopped ('' | payload | environment);
	// HeldSince is when the environment hold began (zero when not held).
	LastStop  string
	HeldSince time.Time
}

// Held reports a publication the environment is holding (it waits): a
// publication being imported again after a hold is not.
func (p Publication) Held() bool { return p.LastStop == "environment" && p.State == "discovered" }

// PublicationFile is one manifest file and what became of it.
type PublicationFile struct {
	PublicationID int64
	Path          string
	Bytes         int64
	SHA256        string
	WantableID    string
	Outcome       string // pending | placed | discarded | unresolved
	DiscardReason string
	LibraryPath   string
	PlacedSHA256  string
	Reason        string
	RemovedAt     time.Time
}

const publicationColumns = `id, COALESCE(download_id,0), generation, kind, path, remote_path, manifest_json, manifest_source, cross_volume, state, cohort, cleanup_approved_at, error, discovered_at, deadline_at, updated_at, cleanup_paused_at, last_stop, hold_since`

func scanPublication(row interface{ Scan(...any) error }) (Publication, error) {
	var p Publication
	var cross int64
	var approved, discovered, deadline, updated, paused, held int64
	err := row.Scan(&p.ID, &p.DownloadID, &p.Generation, &p.Kind, &p.Path, &p.RemotePath, &p.ManifestJSON, &p.ManifestSource, &cross, &p.State, &p.Cohort, &approved, &p.Error, &discovered, &deadline, &updated, &paused, &p.LastStop, &held)
	p.CleanupPausedAt, p.HeldSince = msTime(paused), msTime(held)
	p.CrossVolume = cross != 0
	p.CleanupApprovedAt, p.DiscoveredAt, p.DeadlineAt, p.UpdatedAt = msTime(approved), msTime(discovered), msTime(deadline), msTime(updated)
	return p, err
}

func insertPublicationTx(ctx context.Context, tx *sql.Tx, p PublicationWrite, now int64) (int64, error) {
	if p.Cohort == "" {
		p.Cohort = "live"
	}
	if p.Source == "" {
		p.Source = "none"
	}
	if p.ManifestJSON == "" {
		p.ManifestJSON = "{}"
	}
	cross := 0
	if p.CrossVolume {
		cross = 1
	}
	var download any
	if p.DownloadID != 0 {
		download = p.DownloadID
	}
	// One publication per directory and per (download, generation), ever: a
	// replayed fact finds the row it already made.
	// No deadline until the first import starts (StartPublicationImport):
	// waiting for a worker or for the manifest is not a stall.
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO publications (download_id, generation, kind, path, remote_path, manifest_json, manifest_source, cross_volume, state, cohort, discovered_at, deadline_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,'discovered',?,?,?,?)`,
		download, p.Generation, p.Kind, p.Path, p.RemotePath, p.ManifestJSON, p.Source, cross, p.Cohort, now, 0, now)
	if err != nil {
		return 0, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	var id int64
	var owner sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT id, download_id FROM publications WHERE path=?`, p.Path).Scan(&id, &owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) && inserted == 0 && p.DownloadID != 0 {
			// Ignored on (download, generation) under another path (a
			// changed path mapping): a replay of the publication it made.
			err = tx.QueryRowContext(ctx, `SELECT id FROM publications WHERE download_id=? AND generation=?`, p.DownloadID, p.Generation).Scan(&id)
		}
		return id, err
	}
	if inserted == 0 {
		// A removed publication's directory is gone: a different payload —
		// another generation — published at the same path later is a new
		// publication, never attached to the removed one. A replay of the
		// removed publication itself (same generation) still finds it, so it
		// is not resurrected, and another download naming that generation is
		// a collision, refused below. The removed row and its file records
		// go; the history events keep what it was.
		var state, generation string
		if err := tx.QueryRowContext(ctx, `SELECT state, generation FROM publications WHERE id=?`, id).Scan(&state, &generation); err != nil {
			return 0, err
		}
		if state == "removed" && p.Generation != "" && generation != p.Generation {
			if _, err := tx.ExecContext(ctx, `DELETE FROM publications WHERE id=? AND state='removed'`, id); err != nil {
				return 0, err
			}
			return insertPublicationTx(ctx, tx, p, now)
		}
	}
	if inserted == 0 && p.DownloadID != 0 {
		switch {
		case !owner.Valid:
			// A historical directory with no row, marked at cutover: this
			// download is what produced it.
			if _, err := tx.ExecContext(ctx, `UPDATE publications SET download_id=?, updated_at=? WHERE id=? AND download_id IS NULL`, p.DownloadID, now, id); err != nil {
				return 0, err
			}
		case owner.Int64 != p.DownloadID:
			return 0, &PublicationPathClaimedError{Path: p.Path, Owner: owner.Int64}
		}
	}
	return id, nil
}

// RecordOrphanPublication records, in one write, a committed Runner
// publication Curator has no download row for: the publication (live, no
// row), each manifest file discarded{rejected} with reason, and the state
// import_failed, so the per-file cleanup retires the directory. inserted is
// false when a publication already claims the directory (nothing changes).
func (d *DB) RecordOrphanPublication(ctx context.Context, p PublicationWrite, files []PublicationFile, reason string) (int64, bool, error) {
	if p.DownloadID != 0 {
		return 0, false, fmt.Errorf("an orphan publication has no download row")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM publications WHERE path=?`, p.Path).Scan(&existing); err != nil {
		return 0, false, err
	}
	if existing > 0 {
		return 0, false, nil
	}
	now := time.Now().UnixMilli()
	id, err := insertPublicationTx(ctx, tx, p, now)
	if err != nil {
		return 0, false, err
	}
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO publication_files (publication_id, path, bytes, sha256, outcome, discard_reason, reason, updated_at) VALUES (?,?,?,?,'discarded','rejected',?,?)`,
			id, f.Path, f.Bytes, f.SHA256, reason, now); err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state='import_failed', error=?, updated_at=? WHERE id=?`, reason, now, id); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit()
}

// PublicationPathClaimedError is a publication whose directory another
// download's publication already owns.
type PublicationPathClaimedError struct {
	Path  string
	Owner int64
}

func (e *PublicationPathClaimedError) Error() string {
	return fmt.Sprintf("%s is already the publication of download %d", e.Path, e.Owner)
}

// InsertPublication records a publication (salvage, a late one, or a
// historical directory). It is idempotent on path and (download, generation).
func (d *DB) InsertPublication(ctx context.Context, p PublicationWrite) (int64, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := insertPublicationTx(ctx, tx, p, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// GetPublication reads one publication.
func (d *DB) GetPublication(ctx context.Context, id int64) (Publication, error) {
	p, err := scanPublication(d.R.QueryRowContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// PublicationByPath reads the publication claiming a directory.
func (d *DB) PublicationByPath(ctx context.Context, path string) (Publication, bool, error) {
	p, err := scanPublication(d.R.QueryRowContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE path=?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (d *DB) publicationsWhere(ctx context.Context, where string, args ...any) ([]Publication, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Publication
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPublicationsForDownload lists one row's publications, oldest first.
func (d *DB) ListPublicationsForDownload(ctx context.Context, downloadID int64) ([]Publication, error) {
	return d.publicationsWhere(ctx, `download_id=?`, downloadID)
}

// ListPublicationsForDownloads lists the publications of many rows at once.
func (d *DB) ListPublicationsForDownloads(ctx context.Context, ids []int64) (map[int64][]Publication, error) {
	out := map[int64][]Publication{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	ps, err := d.publicationsWhere(ctx, `download_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		out[p.DownloadID] = append(out[p.DownloadID], p)
	}
	return out, nil
}

// ImportablePublications lists the publications of one client's rows that
// are still waiting to import: the poll offers them again, so a directory
// whose manifest lands later imports then. importing is included: a crash
// mid-import leaves the state behind, and the caller skips a row whose
// import is queued or running (ImportRunning), so only an orphan re-runs.
func (d *DB) ImportablePublications(ctx context.Context, clientID int64) ([]Publication, error) {
	return d.publicationsWhere(ctx, `state IN ('discovered','importing') AND retry_at<=? AND download_id IN (SELECT id FROM downloads WHERE client_id=?)`, time.Now().UnixMilli(), clientID)
}

// OverduePublications lists publications still in discovered/importing past
// their deadline, except one the environment is holding (it waits).
func (d *DB) OverduePublications(ctx context.Context, now time.Time) ([]Publication, error) {
	return d.publicationsWhere(ctx, `state IN ('discovered','importing') AND last_stop<>'environment' AND download_id IS NOT NULL AND deadline_at>0 AND deadline_at<?`, now.UnixMilli())
}

// CountOverduePublications counts what OverduePublications lists.
func (d *DB) CountOverduePublications(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM publications WHERE state IN ('discovered','importing') AND last_stop<>'environment' AND download_id IS NOT NULL AND deadline_at>0 AND deadline_at<?`, now.UnixMilli()).Scan(&n)
	return n, err
}

// AttachPublicationManifest fills in a manifest a publication did not have
// yet (the v1 record arriving on the deliveries feed after the event).
func (d *DB) AttachPublicationManifest(ctx context.Context, id int64, manifestJSON, source string, crossVolume bool) error {
	cross := 0
	if crossVolume {
		cross = 1
	}
	_, err := d.W.ExecContext(ctx, `UPDATE publications SET manifest_json=?, manifest_source=?, cross_volume=?, updated_at=? WHERE id=? AND manifest_source='none' AND state IN ('discovered','importing')`,
		manifestJSON, source, cross, time.Now().UnixMilli(), id)
	return err
}

// SetPublicationState moves a publication and records why.
func (d *DB) SetPublicationState(ctx context.Context, id int64, state, errMsg string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE publications SET state=?, error=?, updated_at=? WHERE id=?`, state, errMsg, time.Now().UnixMilli(), id)
	return err
}

// MaxPublicationImportAttempts bounds how many import attempts of one
// publication may fail on the payload (an error that is not the
// environment's: not a full or blocked library, a cancelled worker or a
// persistence outage, which the six-hour deadline bounds instead). Past it
// the publication ends import_failed and its wantables are grabbed again.
const MaxPublicationImportAttempts = 5

// StartPublicationImport moves a publication to importing and starts its
// stall deadline the first time (or again after an environment hold): six
// hours from that start, never reset by an ordinary retry, so a publication
// that keeps returning to discovered still ends. It returns how many attempts already failed on the
// payload (RequeueFailedPublicationAttempt).
func (d *DB) StartPublicationImport(ctx context.Context, id int64) (int, error) {
	now := time.Now()
	var failed int
	// A publication leaving an environment hold starts its deadline over: the
	// hold was not time spent importing. Its hold stays shown until the
	// attempt ends some other way.
	err := d.W.QueryRowContext(ctx, `UPDATE publications SET state='importing', retry_at=0,
		deadline_at=CASE WHEN deadline_at=0 OR last_stop='environment' THEN ? ELSE deadline_at END, updated_at=? WHERE id=? RETURNING import_attempts`,
		now.Add(DeadlinePublication).UnixMilli(), now.UnixMilli(), id).Scan(&failed)
	return failed, err
}

// RequeueFailedPublicationAttempt returns an importing publication to
// discovered after an attempt that failed on the payload, counts it, and
// spaces the next attempt on the retry ladder (the poll does not offer it
// before retry_at). wasHeld reports that it ended an environment hold.
func (d *DB) RequeueFailedPublicationAttempt(ctx context.Context, id int64, reason string) (wasHeld bool, err error) {
	now := time.Now()
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var was string
	if err := tx.QueryRowContext(ctx, `SELECT last_stop FROM publications WHERE id=? AND state='importing'`, id).Scan(&was); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var attempts int
	if err := tx.QueryRowContext(ctx, `UPDATE publications SET state='discovered', error=?, last_stop='payload', hold_since=0, holds=0, import_attempts=import_attempts+1, updated_at=? WHERE id=? RETURNING import_attempts`,
		reason, now.UnixMilli(), id).Scan(&attempts); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET retry_at=? WHERE id=?`, now.Add(backoff.Retry.After(attempts)).UnixMilli(), id); err != nil {
		return false, err
	}
	return was == "environment", tx.Commit()
}

// HoldPublicationForEnvironment returns a publication to discovered because
// the environment, not the payload, stopped it (a full or blocked library,
// the completed root's storage unavailable or outside the configured roots,
// the manifest unreadable or not there yet). It waits, visibly (held, the
// reason, since when), is never ended by its deadline while that is its last
// stop, and is offered again on the retry ladder (retry_at), not every poll.
// started reports that this begins a hold (it was not held before), so the
// caller records the hold once.
func (d *DB) HoldPublicationForEnvironment(ctx context.Context, id int64, reason string) (started bool, err error) {
	now := time.Now()
	var holds int
	var was string
	if err := d.W.QueryRowContext(ctx, `SELECT last_stop FROM publications WHERE id=?`, id).Scan(&was); err != nil {
		return false, err
	}
	err = d.W.QueryRowContext(ctx, `UPDATE publications SET state='discovered', error=?, last_stop='environment',
		hold_since=CASE WHEN last_stop='environment' AND hold_since>0 THEN hold_since ELSE ? END, holds=holds+1, updated_at=?
		WHERE id=? AND state IN ('discovered','importing') RETURNING holds`, reason, now.UnixMilli(), now.UnixMilli(), id).Scan(&holds)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE publications SET retry_at=? WHERE id=?`, now.Add(backoff.Retry.After(holds)).UnixMilli(), id); err != nil {
		return false, err
	}
	return was != "environment", nil
}

// ReturnPublicationUncounted returns an importing publication to discovered
// after an attempt that stopped for a reason that is neither the payload's
// nor a known environment hold (a busy database, an I/O error, a cancelled
// worker): not counted, bounded by its deadline. A hold it was in is over
// (wasHeld reports it, so the caller records that once).
func (d *DB) ReturnPublicationUncounted(ctx context.Context, id int64, reason string) (wasHeld bool, err error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var was string
	if err := tx.QueryRowContext(ctx, `SELECT last_stop FROM publications WHERE id=? AND state IN ('discovered','importing')`, id).Scan(&was); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state='discovered', error=?, last_stop='', hold_since=0, holds=0, updated_at=? WHERE id=?`,
		reason, time.Now().UnixMilli(), id); err != nil {
		return false, err
	}
	return was == "environment", tx.Commit()
}

// CountHeldPublications counts the publications the environment is holding
// (the System page's pipeline tiles).
func (d *DB) CountHeldPublications(ctx context.Context) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM publications WHERE state='discovered' AND last_stop='environment'`).Scan(&n)
	return n, err
}

// ResetPublicationForRetry starts a publication over for a person's explicit
// retry (Resolve, Retry import): no attempts counted, no deadline yet.
func (d *DB) ResetPublicationForRetry(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, `UPDATE publications SET state='discovered', error='', import_attempts=0, deadline_at=0, last_stop='', hold_since=0, holds=0, retry_at=0, updated_at=? WHERE id=?`,
		time.Now().UnixMilli(), id)
	return err
}

// EndPublicationForRegrab ends a publication Curator could not import
// (stalled past its deadline, or out of attempts): import_failed with the
// reason and — for a completed publication — a replacement grab for its
// row's wantables in the same transaction, so the want is searched again
// with no one deciding anything (the caller fails the row). ok is false when
// the publication had already moved on.
func (d *DB) EndPublicationForRegrab(ctx context.Context, id int64, reason string, dl Download) (bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	var kind string
	if err := tx.QueryRowContext(ctx, `UPDATE publications SET state='import_failed', error=?, last_stop='', hold_since=0, holds=0, updated_at=? WHERE id=? AND state IN ('discovered','importing') RETURNING kind`, reason, now, id).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if kind == "completed" && dl.ID != 0 {
		if len(dl.WantableIDs) > 0 {
			raw, _ := json.Marshal(dl.WantableIDs)
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replacement_grabs (download_id, wantable_ids, created_at, next_at) VALUES (?,?,?,?)`, dl.ID, string(raw), now, now); err != nil {
				return false, err
			}
		}
	}
	return true, tx.Commit()
}

// SeedPublicationFiles records a manifest's files as pending (existing rows
// keep their outcome).
func (d *DB) SeedPublicationFiles(ctx context.Context, publicationID int64, files []PublicationFile) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	for _, f := range files {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO publication_files (publication_id, path, bytes, sha256, updated_at) VALUES (?,?,?,?,?)`,
			publicationID, f.Path, f.Bytes, f.SHA256, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListPublicationFiles lists one publication's files.
func (d *DB) ListPublicationFiles(ctx context.Context, publicationID int64) ([]PublicationFile, error) {
	return d.publicationFilesWhere(ctx, `publication_id=?`, publicationID)
}

// ListPublicationFilesFor reads the files of many publications in one query,
// keyed by publication (Activity lists a page of rows at once).
func (d *DB) ListPublicationFilesFor(ctx context.Context, publicationIDs []int64) (map[int64][]PublicationFile, error) {
	out := map[int64][]PublicationFile{}
	if len(publicationIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(publicationIDs))
	for i, id := range publicationIDs {
		args[i] = id
	}
	files, err := d.publicationFilesWhere(ctx, `publication_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(publicationIDs)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		out[f.PublicationID] = append(out[f.PublicationID], f)
	}
	return out, nil
}

func (d *DB) publicationFilesWhere(ctx context.Context, where string, args ...any) ([]PublicationFile, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT publication_id, path, bytes, sha256, wantable_id, outcome, discard_reason, library_path, placed_sha256, reason, removed_at FROM publication_files WHERE `+where+` ORDER BY publication_id, path`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PublicationFile
	for rows.Next() {
		var f PublicationFile
		var removed int64
		if err := rows.Scan(&f.PublicationID, &f.Path, &f.Bytes, &f.SHA256, &f.WantableID, &f.Outcome, &f.DiscardReason, &f.LibraryPath, &f.PlacedSHA256, &f.Reason, &removed); err != nil {
			return nil, err
		}
		f.RemovedAt = msTime(removed)
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetPublicationFileOutcome records one file's outcome outside an import
// (an operator's place/discard/keep in Resolve).
func (d *DB) SetPublicationFileOutcome(ctx context.Context, f PublicationFile) error {
	res, err := d.W.ExecContext(ctx, `UPDATE publication_files SET outcome=?, discard_reason=?, library_path=?, placed_sha256=?, reason=?, updated_at=? WHERE publication_id=? AND path=?`,
		f.Outcome, f.DiscardReason, f.LibraryPath, f.PlacedSHA256, f.Reason, time.Now().UnixMilli(), f.PublicationID, f.Path)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// PlacedLibraryFilesWithPrefix lists the publication files Curator placed
// into the library under a path prefix (a video's "<dir>/<stem>."): the
// record of the subtitles Curator put beside that video. Files Curator did
// not place have no such row.
func (d *DB) PlacedLibraryFilesWithPrefix(ctx context.Context, prefix string) ([]PublicationFile, error) {
	if prefix == "" {
		return nil, nil
	}
	// A byte range on the indexed column (SQLite compares TEXT bytewise):
	// every path that starts with prefix, whatever its characters.
	upper := []byte(prefix)
	for len(upper) > 0 && upper[len(upper)-1] == 0xff {
		upper = upper[:len(upper)-1]
	}
	var rows *sql.Rows
	var err error
	if len(upper) == 0 {
		rows, err = d.R.QueryContext(ctx, `SELECT publication_id, path, bytes, sha256, library_path, placed_sha256 FROM publication_files
			WHERE outcome='placed' AND library_path>=?`, prefix)
	} else {
		upper[len(upper)-1]++
		rows, err = d.R.QueryContext(ctx, `SELECT publication_id, path, bytes, sha256, library_path, placed_sha256 FROM publication_files
			WHERE outcome='placed' AND library_path>=? AND library_path<?`, prefix, string(upper))
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PublicationFile
	for rows.Next() {
		f := PublicationFile{Outcome: "placed"}
		if err := rows.Scan(&f.PublicationID, &f.Path, &f.Bytes, &f.SHA256, &f.LibraryPath, &f.PlacedSHA256); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// PublicationImportWrite is what one finished publication import records,
// atomically: every file's outcome, the publication's state, the download
// row's advance, the history row, the outbox rows and the timing end.
type PublicationImportWrite struct {
	PublicationID int64
	Files         []PublicationFile
	State         string // imported | import_failed
	Error         string
	// DownloadID/DownloadState advance the row ('' leaves it as it is: a
	// salvage publication of a failed row).
	DownloadID    int64
	DownloadState string
	DownloadError string
	Handoff       *HandoffEntry
	History       []HistoryWrite
	Outbox        []OutboxWrite
	TimingEnds    []TimingStart
	// TimingStarts opens timings the import begins (import_to_cinema_visible
	// starts when the publication is imported).
	TimingStarts []TimingStart
	// Supersede marks other publications' files for the same wantables
	// discarded{superseded}: the current attempt's file is placed.
	Supersede []SupersedeWrite
	// RegrabWantables writes a replacement grab for the row: the publication
	// placed nothing and the library does not hold it.
	RegrabWantables []string
}

// SupersedeWrite names a wantable the current attempt just placed, and where.
type SupersedeWrite struct {
	WantableID  string
	LibraryPath string
}

// ErrPublicationMoved refuses a completion for a publication that is no
// longer being imported (it finished, was ended, or moved on meanwhile).
var ErrPublicationMoved = errors.New("the publication is no longer being imported")

// CompletePublicationImport records a finished import in one transaction.
func (d *DB) CompletePublicationImport(ctx context.Context, w PublicationImportWrite) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	for _, f := range w.Files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO publication_files (publication_id, path, bytes, sha256, wantable_id, outcome, discard_reason, library_path, placed_sha256, reason, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (publication_id, path) DO UPDATE SET wantable_id=excluded.wantable_id, outcome=excluded.outcome, discard_reason=excluded.discard_reason,
				library_path=excluded.library_path, placed_sha256=excluded.placed_sha256, reason=excluded.reason, updated_at=excluded.updated_at`,
			w.PublicationID, f.Path, f.Bytes, f.SHA256, f.WantableID, f.Outcome, f.DiscardReason, f.LibraryPath, f.PlacedSHA256, f.Reason, now); err != nil {
			return err
		}
	}
	for _, s := range w.Supersede {
		// A non-current attempt's file for the same wantable: open ones, and a
		// placed one whose library file the current attempt replaced.
		if _, err := tx.ExecContext(ctx, `UPDATE publication_files SET outcome='discarded', discard_reason='superseded', reason='superseded by the current attempt', updated_at=?
			WHERE wantable_id=? AND publication_id!=? AND removed_at=0
			  AND (outcome IN ('pending','unresolved')
			       OR (outcome='placed' AND (library_path=? OR library_path NOT IN (SELECT path FROM media_files))))`,
			now, s.WantableID, w.PublicationID, s.LibraryPath); err != nil {
			return err
		}
	}
	if w.PublicationID != 0 {
		// Only an import that owns the publication completes it: one that
		// finished, was ended or was held in the meantime is not written
		// again (no second history row, outbox entry or grab).
		res, err := tx.ExecContext(ctx, `UPDATE publications SET state=?, error=?, last_stop='', hold_since=0, holds=0, retry_at=0, updated_at=? WHERE id=? AND state IN ('discovered','importing')`, w.State, w.Error, now, w.PublicationID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrPublicationMoved
		}
	}
	if w.DownloadID != 0 && w.DownloadState != "" {
		entry := "null"
		if w.Handoff != nil {
			raw, _ := json.Marshal(w.Handoff)
			entry = string(raw)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE downloads SET state=?, error=?, progress=1, state_entered_at=?, deadline_at=0,
			handoff_log=CASE WHEN ?='null' THEN handoff_log ELSE json_insert(handoff_log,'$[#]',json(?)) END, updated_at=? WHERE id=?`,
			w.DownloadState, w.DownloadError, now, entry, entry, now, w.DownloadID); err != nil {
			return err
		}
	}
	q := d.Write.WithTx(tx)
	for _, h := range w.History {
		if err := insertHistoryTx(ctx, q, h, now); err != nil {
			return err
		}
	}
	if err := insertOutboxTx(ctx, tx, w.Outbox, now); err != nil {
		return err
	}
	for _, t := range w.TimingEnds {
		// Same semantics as EndPipelineTiming: an end before the start (a
		// backwards clock step) closes the sample at duration 0.
		if _, err := tx.ExecContext(ctx, `UPDATE pipeline_timings SET ended_at=MAX(?,started_at) WHERE kind=? AND transfer=? AND ended_at=0`, now, t.Kind, t.Transfer); err != nil {
			return err
		}
	}
	if err := startTimingsTx(ctx, tx, w.TimingStarts, now); err != nil {
		return err
	}
	if w.DownloadID != 0 && len(w.RegrabWantables) > 0 {
		raw, _ := json.Marshal(w.RegrabWantables)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replacement_grabs (download_id, wantable_ids, created_at, next_at) VALUES (?,?,?,?)`, w.DownloadID, string(raw), now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- precedence -----------------------------------------------------------

// WantableAttempt is the download Curator currently intends for a wantable.
type WantableAttempt struct {
	WantableID string
	DownloadID int64
	DecidedAt  time.Time
	DecidedBy  string
}

// WantableAttempts reads the current attempt of each wantable id given.
func (d *DB) WantableAttempts(ctx context.Context, ids []string) (map[string]WantableAttempt, error) {
	out := map[string]WantableAttempt{}
	for _, id := range ids {
		var a WantableAttempt
		var decided int64
		err := d.R.QueryRowContext(ctx, `SELECT wantable_id, download_id, decided_at, decided_by FROM wantable_attempts WHERE wantable_id=?`, id).Scan(&a.WantableID, &a.DownloadID, &decided, &a.DecidedBy)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		a.DecidedAt = msTime(decided)
		out[id] = a
	}
	return out, nil
}

// SetWantableAttempt records a choice made outside an insert.
func (d *DB) SetWantableAttempt(ctx context.Context, wantableID string, downloadID int64, by string) error {
	_, err := d.W.ExecContext(ctx, `INSERT OR REPLACE INTO wantable_attempts (wantable_id, download_id, decided_at, decided_by) VALUES (?,?,?,?)`,
		wantableID, downloadID, time.Now().UnixMilli(), by)
	return err
}

// LibraryHoldsDownloadFile reports whether the library has a file for the
// given episodes (or, with no episodes, the item and copy) that came from
// the download with this generation key.
func (d *DB) LibraryHoldsDownloadFile(ctx context.Context, itemID, copyID int64, episodeIDs []int64, generationKey string) (bool, error) {
	if generationKey == "" {
		return false, nil
	}
	var n int
	if len(episodeIDs) == 0 {
		err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_files WHERE media_item_id=? AND COALESCE(copy_id,0)=? AND source_generation=?`, itemID, copyID, generationKey).Scan(&n)
		return n > 0, err
	}
	args := []any{itemID, copyID, generationKey}
	for _, e := range episodeIDs {
		args = append(args, e)
	}
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_files f JOIN media_file_episodes e ON e.media_file_id=f.id
		WHERE f.media_item_id=? AND COALESCE(f.copy_id,0)=? AND f.source_generation=? AND e.episode_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(episodeIDs)), ",")+`)`, args...).Scan(&n)
	return n > 0, err
}

// ---- replacement grabs ----------------------------------------------------

// ReplacementGrab is one failed download's next grab, waiting or done.
type ReplacementGrab struct {
	DownloadID  int64
	WantableIDs []string
	CreatedAt   time.Time
	AttemptedAt time.Time
	Attempts    int
	Result      string
	NextAt      time.Time
}

// DueReplacementGrabs lists unsettled grabs whose time has come.
func (d *DB) DueReplacementGrabs(ctx context.Context, now time.Time, limit int) ([]ReplacementGrab, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT download_id, wantable_ids, created_at, attempted_at, attempts, result, next_at FROM replacement_grabs WHERE result='' AND next_at<=? ORDER BY created_at LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReplacementGrab
	for rows.Next() {
		var g ReplacementGrab
		var wants string
		var created, attempted, next int64
		if err := rows.Scan(&g.DownloadID, &wants, &created, &attempted, &g.Attempts, &g.Result, &next); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(wants), &g.WantableIDs)
		g.CreatedAt, g.AttemptedAt, g.NextAt = msTime(created), msTime(attempted), msTime(next)
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetReplacementGrab reads one row's grab.
func (d *DB) GetReplacementGrab(ctx context.Context, downloadID int64) (ReplacementGrab, bool, error) {
	var g ReplacementGrab
	var wants string
	var created, attempted, next int64
	err := d.R.QueryRowContext(ctx, `SELECT download_id, wantable_ids, created_at, attempted_at, attempts, result, next_at FROM replacement_grabs WHERE download_id=?`, downloadID).
		Scan(&g.DownloadID, &wants, &created, &attempted, &g.Attempts, &g.Result, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return g, false, nil
	}
	if err != nil {
		return g, false, err
	}
	_ = json.Unmarshal([]byte(wants), &g.WantableIDs)
	g.CreatedAt, g.AttemptedAt, g.NextAt = msTime(created), msTime(attempted), msTime(next)
	return g, true, nil
}

// ClaimReplacementGrab marks one attempt started, so a crash mid-grab is
// visible as an attempt and a concurrent worker cannot take the same row.
func (d *DB) ClaimReplacementGrab(ctx context.Context, downloadID int64, now time.Time) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE replacement_grabs SET attempted_at=?, attempts=attempts+1, next_at=? WHERE download_id=? AND result='' AND next_at<=?`,
		now.UnixMilli(), now.Add(time.Minute).UnixMilli(), downloadID, now.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SettleReplacementGrab records the grab's result (” with nextAt re-queues).
func (d *DB) SettleReplacementGrab(ctx context.Context, downloadID int64, result string, nextAt time.Time) error {
	_, err := d.W.ExecContext(ctx, `UPDATE replacement_grabs SET result=?, next_at=? WHERE download_id=?`, result, unixMilliOrZero(nextAt), downloadID)
	return err
}

// PendingReplacementWantablesExcept lists the wantables an unsettled grab
// owns (other than one download's own), so the wanted pass does not grab
// them a second time.
func (d *DB) PendingReplacementWantablesExcept(ctx context.Context, exceptDownload int64) (map[string]bool, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT j.value FROM replacement_grabs g, json_each(g.wantable_ids) j WHERE g.result='' AND g.download_id!=?`, exceptDownload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ---- the ordered history cursor -------------------------------------------

// RunnerHistoryCursor is the stored position of the history reader.
type RunnerHistoryCursor struct {
	ClientID       int64
	Incarnation    string
	SinceSeq       int64
	HeadSeq        int64
	RetainedMinSeq int64
	Stored         bool
}

// GetHistoryCursor reads a client's history cursor.
func (d *DB) GetHistoryCursor(ctx context.Context, clientID int64) (RunnerHistoryCursor, error) {
	c := RunnerHistoryCursor{ClientID: clientID}
	err := d.R.QueryRowContext(ctx, `SELECT incarnation, since_seq, head_seq, retained_min_seq, history_read FROM runner_history_cursors WHERE client_id=?`, clientID).
		Scan(&c.Incarnation, &c.SinceSeq, &c.HeadSeq, &c.RetainedMinSeq, &c.Stored)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	return c, err
}

// UpsertHistoryCursor stores a client's history cursor. Only the ordered
// history reader calls it, after the page's terminals are committed.
func (d *DB) UpsertHistoryCursor(ctx context.Context, c RunnerHistoryCursor) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO runner_history_cursors (client_id, incarnation, since_seq, head_seq, retained_min_seq, updated_at, history_read) VALUES (?,?,?,?,?,?,1)
		ON CONFLICT (client_id) DO UPDATE SET incarnation=excluded.incarnation, since_seq=excluded.since_seq, head_seq=excluded.head_seq,
			retained_min_seq=excluded.retained_min_seq, updated_at=excluded.updated_at, history_read=1`,
		c.ClientID, c.Incarnation, c.SinceSeq, c.HeadSeq, c.RetainedMinSeq, time.Now().UnixMilli())
	return err
}

// ---- deadlines --------------------------------------------------------------

// OverdueHeldDownloads lists one client's held rows past their deadline: the
// held (stale) sweep's own query, so other clients' or states' overdue rows
// can never crowd it out.
func (d *DB) OverdueHeldDownloads(ctx context.Context, clientID int64, now time.Time) ([]Download, error) {
	return d.downloadsWhere(ctx, `client_id=? AND state='held' AND runner_outcome='' AND deadline_at>0 AND deadline_at<?`, clientID, now.UnixMilli())
}

// overdueWhere is the contract section 3.5 predicate CountOverdueDownloads
// reports: a non-terminal row past its state's deadline (legacy parked and
// superseded rows excepted). A row still being submitted is the admission
// check's, and a planned one has no transfer yet.
const overdueWhere = `state NOT IN ('imported','notified','failed','planned','submitting') AND deadline_at>0 AND deadline_at<? AND superseded=0 AND parked_at=0`

// OverdueDownloads lists one client's rows past their deadline: the overdue
// exit's own query (every non-terminal state, not only held).
func (d *DB) OverdueDownloads(ctx context.Context, clientID int64, now time.Time) ([]Download, error) {
	return d.downloadsWhere(ctx, `client_id=? AND `+overdueWhere, clientID, now.UnixMilli())
}

// OverdueDownloadsAnyClient lists every client's rows past their deadline.
func (d *DB) OverdueDownloadsAnyClient(ctx context.Context, now time.Time) ([]Download, error) {
	return d.downloadsWhere(ctx, overdueWhere, now.UnixMilli())
}

// ExtendDownloadDeadline moves an overdue row's deadline (its client is
// still progressing it), only while the row is as read.
func (d *DB) ExtendDownloadDeadline(ctx context.Context, dl Download, at time.Time) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET deadline_at=? WHERE id=? AND state=? AND deadline_at=?`,
		at.UnixMilli(), dl.ID, dl.State, dl.DeadlineAt.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FailOverdueDownload ends a row its client can no longer finish: failed,
// blameless (no blocklist), with the reason, a handoff entry and — for the
// wantables given — a replacement grab in the same write. A Runner terminal
// already recorded is kept (only an empty or provisional one becomes
// failed{lost}). Only a row still as read (state and deadline) is ended.
//
// runnerSilent additionally requires that no Runner terminal landed since
// the row was read (only an empty or provisional outcome): a decision drawn
// from Runner's queue never overwrites one.
func (d *DB) FailOverdueDownload(ctx context.Context, dl Download, reason string, regrab []string, entry HandoffEntry, runnerSilent bool) (bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	raw, _ := json.Marshal(entry)
	silent := 0
	if runnerSilent {
		silent = 1
	}
	res, err := tx.ExecContext(ctx, `UPDATE downloads SET state='failed', error=?, deadline_at=0, state_entered_at=?,
		runner_outcome=CASE WHEN runner_outcome='' OR provisional=1 THEN 'failed' ELSE runner_outcome END,
		runner_outcome_reason=CASE WHEN runner_outcome='' OR provisional=1 THEN 'lost' ELSE runner_outcome_reason END,
		runner_outcome_at=CASE WHEN runner_outcome='' OR provisional=1 THEN ? ELSE runner_outcome_at END,
		runner_blames_release=0, provisional=0,
		handoff_log=json_insert(handoff_log,'$[#]',json(?)), updated_at=?
		WHERE id=? AND state=? AND deadline_at=? AND (?=0 OR runner_outcome='' OR provisional=1)`,
		reason, now, now, string(raw), now, dl.ID, dl.State, dl.DeadlineAt.UnixMilli(), silent)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	if len(regrab) > 0 {
		ids, _ := json.Marshal(regrab)
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replacement_grabs (download_id, wantable_ids, created_at, next_at) VALUES (?,?,?,?)`, dl.ID, string(ids), now, now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// CountOverdueDownloads counts the rows past their state's deadline: the
// contract section 3.5 predicate (non-terminal state, deadline_at set and
// past), legacy parked and superseded rows excepted.
func (d *DB) CountOverdueDownloads(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE state NOT IN ('imported','notified','failed') AND deadline_at>0 AND deadline_at<? AND superseded=0 AND parked_at=0`, now.UnixMilli()).Scan(&n)
	return n, err
}

// ExistingPublicationPaths lists every path a publication claims.
func (d *DB) ExistingPublicationPaths(ctx context.Context) (map[string]bool, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT path FROM publications`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// CountHistoricalPublications counts the cohort awaiting the M7 review.
func (d *DB) CountHistoricalPublications(ctx context.Context) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM publications WHERE cohort='historical' AND cleanup_approved_at=0 AND state!='removed'`).Scan(&n)
	return n, err
}

// ---- per-file cleanup (contract section 3.4; M4) ---------------------------

// PublicationsToSweep lists publications the hourly sweep has work for: a
// finished import (imported, notified, or import_failed: a failed one's
// discarded files are garbage once its want is grabbed again) that automatic deletion may touch
// (live, or historical with its review batch approved), with a file whose
// outcome is evidence for deletion and that is still on disk, a file whose
// outcome is still to be decided (pending: a sidecar an earlier build left
// for Resolve, re-decided by the cleanup pass), or with every
// file gone and the directory perhaps left behind. The oldest-touched come
// first so a publication that keeps failing cannot starve the rest.
func (d *DB) PublicationsToSweep(ctx context.Context, limit int) ([]Publication, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE state IN ('imported','notified','import_failed') AND (cohort='live' OR cleanup_approved_at>0) AND cleanup_paused_at=0
		AND (EXISTS (SELECT 1 FROM publication_files f WHERE f.publication_id=publications.id AND f.removed_at=0 AND f.outcome IN ('placed','discarded','pending'))
		     OR NOT EXISTS (SELECT 1 FROM publication_files f WHERE f.publication_id=publications.id AND f.removed_at=0))
		ORDER BY updated_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Publication
	for rows.Next() {
		p, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkPublicationFileRemoved records that a file's bytes left the
// publication directory. Only a file that still has its deleting outcome is
// marked: a concurrent Resolve that changed it wins.
func (d *DB) MarkPublicationFileRemoved(ctx context.Context, publicationID int64, path, outcome string) error {
	res, err := d.W.ExecContext(ctx, `UPDATE publication_files SET removed_at=?, updated_at=? WHERE publication_id=? AND path=? AND outcome=? AND removed_at=0`,
		time.Now().UnixMilli(), time.Now().UnixMilli(), publicationID, path, outcome)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// ClearPublicationCleanupError forgets why an earlier cleanup pass stopped,
// once a pass gets through with nothing stopped. An import's own reasons
// (import_failed) are not cleanup's to clear.
func (d *DB) ClearPublicationCleanupError(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, `UPDATE publications SET error='', updated_at=? WHERE id=? AND (error LIKE 'cleanup stopped:%' OR error LIKE 'cleanup kept %')`, time.Now().UnixMilli(), id)
	return err
}

// SetPublicationCleanupPaused pauses (or resumes) a publication's automatic
// cleanup: the sweep and the post-import pass skip a paused publication.
func (d *DB) SetPublicationCleanupPaused(ctx context.Context, id int64, paused bool) error {
	at := int64(0)
	if paused {
		at = time.Now().UnixMilli()
	}
	res, err := d.W.ExecContext(ctx, `UPDATE publications SET cleanup_paused_at=?, updated_at=? WHERE id=? AND state!='removed'`, at, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// SetPublicationError records why a publication's cleanup stopped, leaving
// its state alone (an imported publication stays imported).
func (d *DB) SetPublicationError(ctx context.Context, id int64, errMsg string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE publications SET error=?, updated_at=? WHERE id=? AND state!='removed'`, errMsg, time.Now().UnixMilli(), id)
	return err
}

// MarkPublicationRemoved ends a publication whose directory is gone. Its
// completed row's payload is then gone too, which is what lets Activity
// retention delete the row (and the publication with it) later.
func (d *DB) MarkPublicationRemoved(ctx context.Context, id int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state='removed', error='', updated_at=? WHERE id=? AND state!='removed'`, now, id); err != nil {
		return err
	}
	var downloadID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT download_id FROM publications WHERE id=? AND kind='completed'`, id).Scan(&downloadID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if downloadID.Valid {
		if _, err := tx.ExecContext(ctx, `UPDATE downloads SET payload_removed=1, updated_at=? WHERE id=?`, now, downloadID.Int64); err != nil {
			return err
		}
		// A row Activity already hid loses its last cleanup obligation here.
		if _, err := d.Write.WithTx(tx).DeleteDismissedImportedDownload(ctx, downloadID.Int64); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DiscardPendingPublicationFile records a pending file discarded, only while
// its publication is still exactly as read (state and updated_at): a
// person's retry or an import that moved the publication on wins.
func (d *DB) DiscardPendingPublicationFile(ctx context.Context, pub Publication, path, reason string) (bool, error) {
	now := time.Now().UnixMilli()
	res, err := d.W.ExecContext(ctx, `UPDATE publication_files SET outcome='discarded', discard_reason='rejected', library_path='', placed_sha256='', reason=?, updated_at=?
		WHERE publication_id=? AND path=? AND outcome='pending' AND removed_at=0
		  AND EXISTS (SELECT 1 FROM publications p WHERE p.id=? AND p.state=? AND p.updated_at=?)`,
		reason, now, pub.ID, path, pub.ID, pub.State, pub.UpdatedAt.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RequeuePublicationForPendingMedia puts an ended publication that still
// holds pending media back to discovered so the import decides it, only
// while the publication is exactly as read and has attempts left. The
// requeue counts as an attempt, so it is bounded: the import's own attempt
// cap ends the publication (deciding every file) when they run out.
func (d *DB) RequeuePublicationForPendingMedia(ctx context.Context, pub Publication) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE publications SET state='discovered', error='', deadline_at=0, last_stop='', retry_at=0, hold_since=0, holds=0,
		import_attempts=import_attempts+1, updated_at=?
		WHERE id=? AND state=? AND updated_at=? AND import_attempts<?`,
		time.Now().UnixMilli(), pub.ID, pub.State, pub.UpdatedAt.UnixMilli(), MaxPublicationImportAttempts)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PublicationImportAttempts reads a publication's counted attempts.
func (d *DB) PublicationImportAttempts(ctx context.Context, id int64) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT import_attempts FROM publications WHERE id=?`, id).Scan(&n)
	return n, err
}

// OfferablePublications lists every publication with a download row waiting
// for, or stuck in, an import, whatever its client: the hourly pass offers
// each one (or ends it when no client can import it), so none waits on the
// poll alone.
func (d *DB) OfferablePublications(ctx context.Context) ([]Publication, error) {
	return d.publicationsWhere(ctx, `state IN ('discovered','importing') AND retry_at<=? AND download_id IS NOT NULL`, time.Now().UnixMilli())
}

// SettleRunnerSalvage records what became of a salvage Runner had reported
// pending (none: it will not come). Only a row still pending is changed.
func (d *DB) SettleRunnerSalvage(ctx context.Context, downloadID int64, salvage string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET runner_salvage=? WHERE id=? AND runner_salvage='pending'`, salvage, downloadID)
	return err
}

// PendingSalvageDownloads lists failed rows whose salvage Runner reported
// pending and that have no salvage publication yet.
func (d *DB) PendingSalvageDownloads(ctx context.Context) ([]Download, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM downloads WHERE state='failed' AND runner_salvage='pending'
		AND NOT EXISTS (SELECT 1 FROM publications p WHERE p.download_id=downloads.id AND p.kind='salvage') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(ids))
	for _, id := range ids {
		dl, err := d.GetDownload(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, dl)
	}
	return out, nil
}

// RecordOnlyPublications lists the publications that are only a record of a
// path: no download row, no manifest and no file — the migration-0040 review
// cohort and directories the cutover froze. Nothing can import or clean them
// file by file; they end when their directory is gone or the path was never
// a payload directory.
func (d *DB) RecordOnlyPublications(ctx context.Context) ([]Publication, error) {
	return d.publicationsWhere(ctx, `download_id IS NULL AND manifest_source='none' AND state IN ('discovered','import_failed')
		AND NOT EXISTS (SELECT 1 FROM publication_files f WHERE f.publication_id=publications.id)`)
}

// DeleteRecordOnlyPublication deletes a record-only publication, freeing
// its path; the caller keeps a history event of what it was. ok is false
// when it is no longer record-only.
func (d *DB) DeleteRecordOnlyPublication(ctx context.Context, id int64) (bool, error) {
	res, err := d.W.ExecContext(ctx, `DELETE FROM publications WHERE id=? AND download_id IS NULL AND manifest_source='none'
		AND state IN ('discovered','import_failed') AND NOT EXISTS (SELECT 1 FROM publication_files f WHERE f.publication_id=publications.id)`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ApprovePublicationCleanup is the M7 approval of a review batch: it lets the
// per-file rule delete the named historical publications' evidenced files.
// Live publications, and ones already approved, are untouched.
func (d *DB) ApprovePublicationCleanup(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := []any{time.Now().UnixMilli()}
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := d.W.ExecContext(ctx, `UPDATE publications SET cleanup_approved_at=? WHERE cohort='historical' AND cleanup_approved_at=0 AND id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListPublicationPaths lists every publication still claiming a directory
// (the completed-storage inventory explains those entries by it).
func (d *DB) ListPublicationPaths(ctx context.Context) ([]Publication, error) {
	return d.publicationsWhere(ctx, `state != 'removed'`)
}

// PublicationStorageFacts is what the completed-storage inventory needs to
// offer the right action on a publication's directory: how many manifest
// files are still on disk per publication, and which absolute file paths an
// operator may discard (unresolved or never imported, not yet removed, on a
// publication automatic deletion may touch).
func (d *DB) PublicationStorageFacts(ctx context.Context) (map[int64]int, map[string]bool, error) {
	unremoved := map[int64]int{}
	rows, err := d.R.QueryContext(ctx, `SELECT publication_id, COUNT(*) FROM publication_files WHERE removed_at=0 GROUP BY publication_id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		unremoved[id] = n
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	discardable := map[string]bool{}
	rows, err = d.R.QueryContext(ctx, `SELECT p.path, f.path FROM publication_files f JOIN publications p ON p.id=f.publication_id
		WHERE f.removed_at=0 AND f.outcome IN ('unresolved','pending') AND p.state NOT IN ('removed','discovered','importing')
		  AND (p.cohort='live' OR p.cleanup_approved_at>0)`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var dir, rel string
		if err := rows.Scan(&dir, &rel); err != nil {
			return nil, nil, err
		}
		discardable[filepath.Join(dir, filepath.FromSlash(rel))] = true
	}
	return unremoved, discardable, rows.Err()
}
