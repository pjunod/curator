package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
	"github.com/pjunod/monarr/internal/ports"
)

// Contract v2 (M2) keeps every write a terminal or an import implies in one
// transaction. These matrices fail each storage call of the reducer's and
// the publication importer's stores in turn and require two things: the
// injected failure is what the caller sees (never a success, never another
// error), and a transactional write leaves no partial state behind.

// pipelineSnapshot captures every table except goose's own bookkeeping.
func pipelineSnapshot(t *testing.T, db *DB) string {
	t.Helper()
	rows, err := db.R.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='goose_db_version' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	all := map[string][]any{}
	for _, table := range tables {
		rows, err := db.R.QueryContext(t.Context(), `SELECT * FROM "`+table+`" ORDER BY rowid`)
		if err != nil {
			// WITHOUT ROWID tables have no rowid.
			rows, err = db.R.QueryContext(t.Context(), `SELECT * FROM "`+table+`"`)
			if err != nil {
				t.Fatal(err)
			}
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			all[table] = append(all[table], values)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// pipelineFixture is one Runner-correlated download mid-flight, its client's
// indexer, and a second (failed) download with its own publication, grab and
// cursor: enough durable state for every v2 store method to have rows.
type pipelineFixture struct {
	db          *DB
	item        int64
	indexer     int64
	dl          Download // downloading, open terminal, transfer "tok-live"
	failed      Download // failed, owns a publication and a replacement grab
	publication int64    // failed's publication, overdue, with files
}

func newPipelineFixture(t *testing.T) pipelineFixture {
	t.Helper()
	ctx := context.Background()
	db := openMigratedCopy(t)
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Pipeline v2 store", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	indexer, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "idx", URL: "http://idx", APIKey: "k", Protocol: "usenet", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: item, State: "downloading", ReleaseTitle: "Live", Protocol: "usenet", ClientID: 7, Handle: "h-live", Transfer: "tok-live"})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: item, State: "failed", ReleaseTitle: "Failed", Protocol: "usenet", ClientID: 7, Handle: "h-failed", Transfer: "tok-failed"})
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE downloads SET state='held', deadline_at=? WHERE id=?`, []any{past, dl.ID}},
		{`INSERT INTO publications (download_id, generation, kind, path, remote_path, state, cohort, discovered_at, deadline_at, updated_at) VALUES (?, 'g1', 'salvage', '/completed/failed', '/remote/failed', 'discovered', 'live', 1, ?, 1)`, []any{failed.ID, past}},
		{`INSERT INTO publications (generation, kind, path, state, cohort, discovered_at, updated_at) VALUES ('hist', 'completed', '/completed/historical', 'discovered', 'historical', 1, 1)`, nil},
		{`INSERT INTO replacement_grabs (download_id, wantable_ids, created_at, next_at) VALUES (?, '["movie:1"]', 1, 1)`, []any{failed.ID}},
		{`INSERT OR REPLACE INTO wantable_attempts (wantable_id, download_id, decided_at, decided_by) VALUES ('movie:1', ?, 1, 'grab')`, []any{failed.ID}},
		{`INSERT INTO runner_history_cursors (client_id, incarnation, since_seq, head_seq, retained_min_seq, updated_at) VALUES (7, 'inc', 3, 9, 1, 1)`, nil},
		{`INSERT INTO media_files (media_item_id, path, size, added_at, source_generation) VALUES (?, '/library/movie.mkv', 1, 1, ?)`, []any{item, failed.GenerationKey}},
	} {
		if _, err := db.W.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	pub, ok, err := db.PublicationByPath(ctx, "/completed/failed")
	if err != nil || !ok {
		t.Fatalf("fixture publication: %v %v", ok, err)
	}
	if err := db.SeedPublicationFiles(ctx, pub.ID, []PublicationFile{{Path: "a.mkv", Bytes: 1, SHA256: "aa"}, {Path: "b.nfo", Bytes: 2, SHA256: "bb"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE publication_files SET wantable_id='movie:1' WHERE publication_id=? AND path='a.mkv'`, pub.ID); err != nil {
		t.Fatal(err)
	}
	if dl, err = db.GetDownload(ctx, dl.ID); err != nil {
		t.Fatal(err)
	}
	if failed, err = db.GetDownload(ctx, failed.ID); err != nil {
		t.Fatal(err)
	}
	return pipelineFixture{db: db, item: item, indexer: indexer, dl: dl, failed: failed, publication: pub.ID}
}

// withFaultyConn swaps one side of the pool for a connection that fails its
// at-th driver call, runs op, and restores the pool.
func withFaultyConn(t *testing.T, db *DB, write bool, fault *importFaultDriver, op func() error) error {
	t.Helper()
	name := fmt.Sprintf("pipeline-v2-fault-%d", importFaultDriverID.Add(1))
	sql.Register(name, fault)
	dsn := "file:" + db.Path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	if !write {
		dsn = "file:" + db.Path + "?_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	}
	conn, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	if write {
		origW, origQ := db.W, db.Write
		db.W, db.Write = conn, sqlitegen.New(conn)
		defer func() { db.W, db.Write = origW, origQ }()
	} else {
		origR, origQ := db.R, db.Read
		db.R, db.Read = conn, sqlitegen.New(conn)
		defer func() { db.R, db.Read = origR, origQ }()
	}
	err = op()
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return err
}

func TestPipelineV2StoreWriteFailuresAreReportedAndAtomic(t *testing.T) {
	type writeCase struct {
		atomic bool // the write is one transaction: a failure leaves nothing
		run    func(f pipelineFixture) error
	}
	ctx := context.Background()
	cases := map[string]writeCase{
		"terminal-full": {atomic: true, run: func(f pipelineFixture) error {
			won, err := f.db.RecordRunnerTerminal(ctx, RunnerTerminalWrite{
				DownloadID: f.dl.ID, Transfer: f.dl.Transfer, Outcome: "failed", Reason: "fetch", FetchCause: "indexer_unavailable", BlamesRelease: true,
				State: "failed", Error: "fetch failed", HistorySeq: 4, SavePath: "/save", ImportPath: "/import", DeadlineAt: time.Now().Add(time.Hour),
				Handoff: &HandoffEntry{Step: "failed", At: 1}, ReplanPlanID: 999, CancelPlanID: 998,
				Blocklist:       &BlocklistWrite{MediaItemID: f.item, SourceIndexerID: f.indexer, SourceGUID: "guid-1", Reason: "fetch"},
				Publication:     &PublicationWrite{DownloadID: f.dl.ID, Generation: "g-live", Kind: "salvage", Path: "/completed/live", RemotePath: "/remote/live"},
				ReplacementGrab: &ReplacementGrabWrite{WantableIDs: []string{"movie:2"}},
				History:         []HistoryWrite{{Type: "download_failed", MediaItemID: f.item, ReleaseTitle: "Live", Data: map[string]any{"reason": "fetch"}}, {Type: "note", MediaItemID: f.item}},
				TimingStarts:    []TimingStart{{Kind: TimingFailureToNextGrab, Transfer: f.dl.Transfer, DownloadID: f.dl.ID}},
				IndexerOutage:   &IndexerOutageWrite{IndexerID: f.indexer, Until: time.Now().Add(time.Hour)},
			})
			if err == nil && !won {
				return errors.New("open terminal lost its once-guard")
			}
			return err
		}},
		"terminal-title-blocklist": {atomic: true, run: func(f pipelineFixture) error {
			_, err := f.db.RecordRunnerTerminal(ctx, RunnerTerminalWrite{DownloadID: f.dl.ID, Transfer: f.dl.Transfer, Outcome: "failed", State: "failed",
				Blocklist:     &BlocklistWrite{MediaItemID: f.item, Title: "Live", Indexer: "idx", Reason: "bad"},
				IndexerOutage: &IndexerOutageWrite{Until: time.Now().Add(time.Hour)}})
			return err
		}},
		"terminal-claimed-publication": {atomic: true, run: func(f pipelineFixture) error {
			_, err := f.db.RecordRunnerTerminal(ctx, RunnerTerminalWrite{DownloadID: f.dl.ID, Transfer: f.dl.Transfer, Outcome: "completed", State: "downloaded",
				Publication: &PublicationWrite{DownloadID: f.dl.ID, Generation: "g-live", Kind: "completed", Path: "/completed/failed"}})
			return err
		}},
		"publication-adopt-historical": {atomic: true, run: func(f pipelineFixture) error {
			_, err := f.db.InsertPublication(ctx, PublicationWrite{DownloadID: f.dl.ID, Generation: "hist", Kind: "completed", Path: "/completed/historical", CrossVolume: true})
			return err
		}},
		"seed-files": {atomic: true, run: func(f pipelineFixture) error {
			return f.db.SeedPublicationFiles(ctx, f.publication, []PublicationFile{{Path: "c.mkv", Bytes: 3}, {Path: "d.mkv", Bytes: 4}})
		}},
		"complete-import": {atomic: true, run: func(f pipelineFixture) error {
			return f.db.CompletePublicationImport(ctx, PublicationImportWrite{
				PublicationID: f.publication, State: "imported",
				Files:      []PublicationFile{{Path: "a.mkv", Bytes: 1, SHA256: "aa", WantableID: "movie:9", Outcome: "placed", LibraryPath: "/library/movie.mkv", PlacedSHA256: "aa"}, {Path: "b.nfo", Bytes: 2, Outcome: "discarded", DiscardReason: "rejected"}},
				Supersede:  []SupersedeWrite{{WantableID: "movie:1", LibraryPath: "/library/movie.mkv"}},
				DownloadID: f.failed.ID, DownloadState: "imported", Handoff: &HandoffEntry{Step: "imported", At: 1},
				History:    []HistoryWrite{{Type: "imported", MediaItemID: f.item}},
				Outbox:     []OutboxWrite{{NotifierID: 1, DownloadID: f.failed.ID, Event: "import", Payload: "{}"}},
				TimingEnds: []TimingStart{{Kind: TimingRunnerDoneToImport, Transfer: f.failed.Transfer}},
			})
		}},
		"fail-stalled": {run: func(f pipelineFixture) error {
			_, err := f.db.FailStalledPublication(ctx, f.publication, "deadline")
			return err
		}},
		"file-outcome": {run: func(f pipelineFixture) error {
			return f.db.SetPublicationFileOutcome(ctx, PublicationFile{PublicationID: f.publication, Path: "b.nfo", Outcome: "discarded", DiscardReason: "rejected"})
		}},
		"claim-grab": {run: func(f pipelineFixture) error {
			_, err := f.db.ClaimReplacementGrab(ctx, f.failed.ID, time.Now())
			return err
		}},
		"wantable": {run: func(f pipelineFixture) error { return f.db.SetWantableAttempt(ctx, "movie:3", f.dl.ID, "operator") }},
		"runner-status": {run: func(f pipelineFixture) error {
			_, err := f.db.UpdateRunnerStatus(ctx, RunnerStatusWrite{DownloadID: f.dl.ID, Transfer: f.dl.Transfer, State: "downloading", Progress: 0.5, Stage: "unpack", Handoff: &HandoffEntry{Step: "downloading"}})
			return err
		}},
		"attach-manifest": {run: func(f pipelineFixture) error {
			return f.db.AttachPublicationManifest(ctx, f.publication, `{"files":[]}`, "file", true)
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			eachWriteFault(t, func(t *testing.T, fault *importFaultDriver) {
				f := newPipelineFixture(t)
				before := pipelineSnapshot(t, f.db)
				err := withFaultyConn(t, f.db, true, fault, func() error { return c.run(f) })
				requireWriteFaultOutcome(t, fault, c.atomic, err, before, pipelineSnapshot(t, f.db))
			})
		})
	}
}

func TestPipelineV2StoreReadFailuresNeverBecomeEmptySuccess(t *testing.T) {
	ctx := context.Background()
	f := newPipelineFixture(t)
	db := f.db
	now := time.Now()
	found := func(ok bool) int {
		if ok {
			return 1
		}
		return 0
	}
	calls := map[string]func() (int, error){
		"by-transfer":   func() (int, error) { _, ok, e := db.DownloadByTransfer(ctx, 7, f.dl.Transfer); return found(ok), e },
		"by-handle":     func() (int, error) { _, ok, e := db.DownloadByHandle(ctx, 7, f.dl.Handle); return found(ok), e },
		"outstanding":   func() (int, error) { v, e := db.OutstandingRunnerDownloads(ctx, 7); return len(v), e },
		"overdue-held":  func() (int, error) { v, e := db.OverdueHeldDownloads(ctx, 7, now); return len(v), e },
		"count-overdue": func() (int, error) { return db.CountOverdueDownloads(ctx, now) },
		"publication":   func() (int, error) { p, e := db.GetPublication(ctx, f.publication); return int(p.ID), e },
		"by-path":       func() (int, error) { _, ok, e := db.PublicationByPath(ctx, "/completed/failed"); return found(ok), e },
		"for-download":  func() (int, error) { v, e := db.ListPublicationsForDownload(ctx, f.failed.ID); return len(v), e },
		"for-downloads": func() (int, error) {
			v, e := db.ListPublicationsForDownloads(ctx, []int64{f.failed.ID, f.dl.ID})
			return len(v[f.failed.ID]), e
		},
		"importable":         func() (int, error) { v, e := db.ImportablePublications(ctx, 7); return len(v), e },
		"overdue-pubs":       func() (int, error) { v, e := db.OverduePublications(ctx, now); return len(v), e },
		"count-overdue-pubs": func() (int, error) { return db.CountOverduePublications(ctx, now) },
		"files":              func() (int, error) { v, e := db.ListPublicationFiles(ctx, f.publication); return len(v), e },
		"files-for": func() (int, error) {
			v, e := db.ListPublicationFilesFor(ctx, []int64{f.publication})
			return len(v[f.publication]), e
		},
		"wantables": func() (int, error) {
			v, e := db.WantableAttempts(ctx, []string{"movie:none", "movie:1"})
			return len(v), e
		},
		"library-holds": func() (int, error) {
			ok, e := db.LibraryHoldsDownloadFile(ctx, f.item, 0, nil, f.failed.GenerationKey)
			return found(ok), e
		},
		"due-grabs":        func() (int, error) { v, e := db.DueReplacementGrabs(ctx, now, 10); return len(v), e },
		"grab":             func() (int, error) { _, ok, e := db.GetReplacementGrab(ctx, f.failed.ID); return found(ok), e },
		"pending-wantable": func() (int, error) { v, e := db.PendingReplacementWantablesExcept(ctx, f.dl.ID); return len(v), e },
		"cursor":           func() (int, error) { c, e := db.GetHistoryCursor(ctx, 7); return found(c.Stored), e },
		"paths":            func() (int, error) { v, e := db.ExistingPublicationPaths(ctx); return len(v), e },
		"historical":       func() (int, error) { return db.CountHistoricalPublications(ctx) },
	}
	before := pipelineSnapshot(t, db)
	for name, call := range calls {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%t", name, malformed), func(t *testing.T) {
				completed := false
				for point := 1; point <= 32 && !completed; point++ {
					fault := &importFaultDriver{at: point, malformed: malformed}
					var count int
					err := withFaultyConn(t, db, false, fault, func() error {
						var e error
						count, e = call()
						return e
					})
					if !fault.fired {
						if err != nil || count == 0 {
							t.Fatalf("unfaulted populated read returned %d, %v", count, err)
						}
						completed = true
						continue
					}
					if err == nil {
						t.Fatalf("read failure %d returned success with %d rows", point, count)
					}
				}
				if !completed {
					t.Fatal("read never completed without injection")
				}
			})
		}
	}
	if after := pipelineSnapshot(t, db); after != before {
		t.Fatal("a read changed durable state")
	}
}

// eachWriteFault runs op once unfaulted on a fresh fixture to count its
// writer calls, then once per call with that call failing, in parallel.
// run builds the fixture, injects fault with withFaultyConn and asserts.
func eachWriteFault(t *testing.T, run func(t *testing.T, fault *importFaultDriver)) {
	t.Helper()
	probe := &importFaultDriver{at: -1}
	t.Run("unfaulted", func(t *testing.T) { run(t, probe) })
	if probe.calls == 0 {
		t.Fatal("operation made no writer calls")
	}
	for point := 1; point <= probe.calls; point++ {
		t.Run(fmt.Sprintf("fail-%d", point), func(t *testing.T) {
			t.Parallel()
			run(t, &importFaultDriver{at: point})
		})
	}
}

// requireWriteFaultOutcome is the write matrices' shared assertion.
func requireWriteFaultOutcome(t *testing.T, fault *importFaultDriver, atomic bool, err error, before, after string) {
	t.Helper()
	if !fault.fired {
		if err != nil {
			t.Fatalf("unfaulted write failed: %v", err)
		}
		if after == before {
			t.Fatal("unfaulted write changed nothing")
		}
		return
	}
	if !errors.Is(err, errImportStorageFault) {
		t.Fatalf("storage failure %d surfaced as %v, not the failure", fault.at, err)
	}
	if atomic && after != before {
		t.Fatalf("storage failure %d left a partial write", fault.at)
	}
}
