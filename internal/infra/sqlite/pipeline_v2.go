package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(ids))
	for _, id := range ids {
		dl, err := d.GetDownload(ctx, id)
		if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
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
		if _, err := insertPublicationTx(ctx, tx, *p, now); err != nil {
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
		deadline_at=?,
		handoff_log=CASE WHEN ?='null' THEN handoff_log ELSE json_insert(handoff_log,'$[#]',json(?)) END,
		updated_at=?
		WHERE id=? AND transfer=? AND runner_outcome='' AND state IN ('grabbed','submitting','downloading','held')`,
		w.State, now, w.State, w.Progress, w.Error, w.Control, w.Control, unixMilliOrZero(w.DeadlineAt), entry, entry, now, w.DownloadID, w.Transfer)
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
}

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

const publicationColumns = `id, COALESCE(download_id,0), generation, kind, path, remote_path, manifest_json, manifest_source, cross_volume, state, cohort, cleanup_approved_at, error, discovered_at, deadline_at, updated_at, cleanup_paused_at`

func scanPublication(row interface{ Scan(...any) error }) (Publication, error) {
	var p Publication
	var cross int64
	var approved, discovered, deadline, updated, paused int64
	err := row.Scan(&p.ID, &p.DownloadID, &p.Generation, &p.Kind, &p.Path, &p.RemotePath, &p.ManifestJSON, &p.ManifestSource, &cross, &p.State, &p.Cohort, &approved, &p.Error, &discovered, &deadline, &updated, &paused)
	p.CleanupPausedAt = msTime(paused)
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
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO publications (download_id, generation, kind, path, remote_path, manifest_json, manifest_source, cross_volume, state, cohort, discovered_at, deadline_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,'discovered',?,?,?,?)`,
		download, p.Generation, p.Kind, p.Path, p.RemotePath, p.ManifestJSON, p.Source, cross, p.Cohort, now, now+DeadlinePublication.Milliseconds(), now); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM publications WHERE path=?`, p.Path).Scan(&id)
	return id, err
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
// without a manifest yet is re-checked rather than forgotten.
func (d *DB) ImportablePublications(ctx context.Context, clientID int64) ([]Publication, error) {
	return d.publicationsWhere(ctx, `state='discovered' AND download_id IN (SELECT id FROM downloads WHERE client_id=?)`, clientID)
}

// OverduePublications lists publications still in discovered/importing past
// their deadline.
func (d *DB) OverduePublications(ctx context.Context, now time.Time) ([]Publication, error) {
	return d.publicationsWhere(ctx, `state IN ('discovered','importing') AND download_id IS NOT NULL AND deadline_at>0 AND deadline_at<?`, now.UnixMilli())
}

// CountOverduePublications counts what OverduePublications lists.
func (d *DB) CountOverduePublications(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM publications WHERE state IN ('discovered','importing') AND download_id IS NOT NULL AND deadline_at>0 AND deadline_at<?`, now.UnixMilli()).Scan(&n)
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

// FailStalledPublication ends a publication that sat in discovered/importing
// past its deadline. It is Curator's own work, so nothing re-opens it.
func (d *DB) FailStalledPublication(ctx context.Context, id int64, reason string) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE publications SET state='import_failed', error=?, updated_at=? WHERE id=? AND state IN ('discovered','importing')`, reason, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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
	rows, err := d.R.QueryContext(ctx, `SELECT publication_id, path, bytes, sha256, wantable_id, outcome, discard_reason, library_path, placed_sha256, reason, removed_at FROM publication_files WHERE publication_id=? ORDER BY path`, publicationID)
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
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// OutboxWrite is one notifier_deliveries row written with the import.
type OutboxWrite struct {
	NotifierID int64
	DownloadID int64
	Event      string
	Payload    string
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
	// Supersede marks other publications' files for the same wantables
	// discarded{superseded}: the current attempt's file is placed.
	Supersede []SupersedeWrite
}

// SupersedeWrite names a wantable the current attempt just placed, and where.
type SupersedeWrite struct {
	WantableID  string
	LibraryPath string
}

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
	if _, err := tx.ExecContext(ctx, `UPDATE publications SET state=?, error=?, updated_at=? WHERE id=?`, w.State, w.Error, now, w.PublicationID); err != nil {
		return err
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
	for _, o := range w.Outbox {
		if _, err := tx.ExecContext(ctx, `INSERT INTO notifier_deliveries (notifier_id, download_id, event, payload, next_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
			o.NotifierID, o.DownloadID, o.Event, o.Payload, now, now, now); err != nil {
			return err
		}
	}
	for _, t := range w.TimingEnds {
		// Same semantics as EndPipelineTiming: an end before the start (a
		// backwards clock step) closes the sample at duration 0.
		if _, err := tx.ExecContext(ctx, `UPDATE pipeline_timings SET ended_at=MAX(?,started_at) WHERE kind=? AND transfer=? AND ended_at=0`, now, t.Kind, t.Transfer); err != nil {
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

// OverdueDownloads lists rows past their state's deadline: the contract
// section 3.5 predicate (non-terminal state, deadline_at set and past).
func (d *DB) OverdueDownloads(ctx context.Context, now time.Time, limit int) ([]Download, error) {
	rows, err := d.downloadsWhere(ctx, `state NOT IN ('imported','notified','failed') AND deadline_at>0 AND deadline_at<?`, now.UnixMilli())
	if err != nil || limit <= 0 || len(rows) <= limit {
		return rows, err
	}
	return rows[:limit], nil
}

// CountOverdueDownloads counts what OverdueDownloads lists.
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
// finished import (imported or notified) that automatic deletion may touch
// (live, or historical with its review batch approved), with a file whose
// outcome is evidence for deletion and that is still on disk, or with every
// file gone and the directory perhaps left behind. The oldest-touched come
// first so a publication that keeps failing cannot starve the rest.
func (d *DB) PublicationsToSweep(ctx context.Context, limit int) ([]Publication, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT `+publicationColumns+` FROM publications WHERE state IN ('imported','notified') AND (cohort='live' OR cleanup_approved_at>0) AND cleanup_paused_at=0
		AND (EXISTS (SELECT 1 FROM publication_files f WHERE f.publication_id=publications.id AND f.removed_at=0 AND f.outcome IN ('placed','discarded'))
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

// DemotePublicationFile turns a file whose evidence no longer holds (its
// bytes or its library copy differ from the manifest) into an unresolved one,
// retained and shown in Resolve, so the sweep stops re-reading it.
func (d *DB) DemotePublicationFile(ctx context.Context, publicationID int64, path, fromOutcome, reason string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE publication_files SET outcome='unresolved', discard_reason='', reason=?, updated_at=? WHERE publication_id=? AND path=? AND outcome=? AND removed_at=0`,
		reason, time.Now().UnixMilli(), publicationID, path, fromOutcome)
	return err
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
