package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

// The acquisition store backs Activity, the queue, profiles and the manual
// grab and import admissions. A storage failure must reach the caller as the
// failure itself, a transactional write must leave nothing half-applied,
// and a guarded delete must never let a failed check count as a pass.

type acquisitionStoreFixture struct {
	db       *DB
	item     int64
	profile  int64
	indexer  int64
	client   int64
	imported Download // imported, payload still on the client
	stopped  Download // failed with "import stopped:", retryable
}

func newAcquisitionStoreFixture(t *testing.T) acquisitionStoreFixture {
	t.Helper()
	ctx := context.Background()
	db := openMigratedCopy(t)
	profile, err := db.AddProfile(ctx, quality.Profile{Name: "Spare", Target: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, UpgradesAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Store", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	indexer, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "idx", URL: "http://idx", Protocol: "usenet", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	client, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "qbittorrent", Name: "qb", URL: "http://qb", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: item, State: "imported", ReleaseTitle: "Store.Imported", Protocol: "usenet", ClientID: client, Handle: "h1", Transfer: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: item, State: "failed", ReleaseTitle: "Store.Stopped", Protocol: "usenet", ClientID: client, Handle: "h2", Transfer: "t2", Error: "import stopped: disk full", ImportPath: "/completed/stopped"})
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, item, 0, "/library/store.mkv", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQuality(ctx, fileID, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddHistory(ctx, "grabbed", item, "Store.Imported", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET error='import stopped: disk full', import_path='/completed/stopped', delivery_expected=0 WHERE id=?`, stopped.ID); err != nil {
		t.Fatal(err)
	}
	if imported, err = db.GetDownload(ctx, imported.ID); err != nil {
		t.Fatal(err)
	}
	if stopped, err = db.GetDownload(ctx, stopped.ID); err != nil {
		t.Fatal(err)
	}
	return acquisitionStoreFixture{db: db, item: item, profile: profile, indexer: indexer, client: client, imported: imported, stopped: stopped}
}

func TestAcquisitionStoreWriteFailuresAreReportedAndAtomic(t *testing.T) {
	ctx := context.Background()
	type writeCase struct {
		atomic bool
		setup  func(f acquisitionStoreFixture) error
		run    func(f acquisitionStoreFixture) error
	}
	cases := map[string]writeCase{
		"add-profile": {run: func(f acquisitionStoreFixture) error {
			_, err := f.db.AddProfile(ctx, quality.Profile{Name: "New", Target: quality.Quality{Source: quality.SourceWEBDL, Resolution: 720}})
			return err
		}},
		"update-profile": {run: func(f acquisitionStoreFixture) error {
			return f.db.UpdateProfile(ctx, quality.Profile{ID: f.profile, Name: "Renamed", Target: quality.Quality{Source: quality.SourceWEBDL, Resolution: 720}})
		}},
		"delete-profile": {run: func(f acquisitionStoreFixture) error { return f.db.DeleteProfile(ctx, f.profile) }},
		"add-indexer": {run: func(f acquisitionStoreFixture) error {
			_, err := f.db.AddIndexer(ctx, ports.IndexerConfig{Name: "idx2", URL: "http://idx2", Protocol: "torrent", Categories: []int{2000}, Enabled: true})
			return err
		}},
		"delete-indexer":  {run: func(f acquisitionStoreFixture) error { return f.db.DeleteIndexer(ctx, f.indexer) }},
		"payload-removed": {atomic: true, run: func(f acquisitionStoreFixture) error { return f.db.MarkPayloadRemoved(ctx, f.imported.ID) }},
		"delete-client": {run: func(f acquisitionStoreFixture) error {
			id, err := f.db.AddDownloadClient(ctx, ports.ClientConfig{Type: "sabnzbd", Name: "spare", URL: "http://sab", Enabled: true})
			if err != nil {
				return err
			}
			return f.db.DeleteDownloadClient(ctx, id)
		}},
		"insert-replacement-download": {run: func(f acquisitionStoreFixture) error {
			_, err := f.db.InsertDownloadRecord(ctx, Download{MediaItemID: f.item, State: "grabbed", ReleaseTitle: "Store.Replacement", Protocol: "usenet", ReplacementOperationID: "op-1"})
			return err
		}},
		"manual-additional": {atomic: true, run: func(f acquisitionStoreFixture) error {
			_, err := f.db.InsertManualAdditionalDownload(ctx, domain.MediaCopy{MediaItemID: f.item, Name: "4K", QualityProfileID: f.profile, Path: "/library/store-4k"},
				Download{MediaItemID: f.item, ReleaseTitle: "Store.2160p", Protocol: "usenet", State: "grabbed", WantableIDs: []string{"movie:1"}, RequestID: "manual-1", RequestFingerprint: "fp"},
				func(id int64) (string, error) { return fmt.Sprintf("manual-%d", id), nil })
			return err
		}},
		"manual-submission": {atomic: true, setup: func(f acquisitionStoreFixture) error {
			_, err := f.db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='submitting', intent='manual_additional' WHERE id=?`, f.stopped.ID)
			return err
		}, run: func(f acquisitionStoreFixture) error {
			return f.db.CommitManualSubmission(ctx, f.stopped.ID, f.stopped.GenerationKey, "client-handle", f.stopped.Transfer)
		}},
		"manual-import-fresh": {atomic: true, run: func(f acquisitionStoreFixture) error {
			_, err := f.db.CommitManualImportAdmission(ctx, Download{MediaItemID: f.item, ReleaseTitle: "Store.Manual", Protocol: "usenet", State: "importing", ImportPath: "/completed/manual", Handoff: []HandoffEntry{{Step: "manual", At: 1}}})
			return err
		}},
		"control": {run: func(f acquisitionStoreFixture) error {
			d := f.stopped
			d.RunnerControl = "paused"
			return f.db.UpdateDownloadControl(ctx, d)
		}},
		"target": {run: func(f acquisitionStoreFixture) error {
			d := f.stopped
			d.ImportPath = "/completed/other"
			return f.db.UpdateDownloadTarget(ctx, d)
		}},
		"handoff-step": {run: func(f acquisitionStoreFixture) error {
			return f.db.AppendHandoffStep(ctx, f.imported.ID, "notified", "plurx")
		}},
		"delete":         {run: func(f acquisitionStoreFixture) error { return f.db.DeleteDownload(ctx, f.stopped.ID) }},
		"clear-failed":   {atomic: true, run: func(f acquisitionStoreFixture) error { _, err := f.db.ClearFailedDownloads(ctx); return err }},
		"clear-imported": {atomic: true, run: func(f acquisitionStoreFixture) error { _, err := f.db.ClearImportedDownloads(ctx); return err }},
		"prune-terminal": {atomic: true, run: func(f acquisitionStoreFixture) error {
			_, err := f.db.PruneTerminalDownloads(ctx, time.Now().Add(time.Hour))
			return err
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			eachWriteFault(t, func(t *testing.T, fault *importFaultDriver) {
				f := newAcquisitionStoreFixture(t)
				if c.setup != nil {
					if err := c.setup(f); err != nil {
						t.Fatal(err)
					}
				}
				before := pipelineSnapshot(t, f.db)
				err := withFaultyConn(t, f.db, true, fault, func() error { return c.run(f) })
				requireWriteFaultOutcome(t, fault, c.atomic, err, before, pipelineSnapshot(t, f.db))
			})
		})
	}
}

// DeleteProfile refuses to delete a kind's default profile. Failing the read
// of the defaults must refuse too, not let the delete through.
func TestAcquisitionStoreDeleteProfileDefaultCheckFailsClosed(t *testing.T) {
	ctx := context.Background()
	for point := 1; point <= 32; point++ {
		f := newAcquisitionStoreFixture(t)
		if err := f.db.SetMeta(ctx, DefaultProfileSetting(domain.KindMovie), fmt.Sprint(f.profile)); err != nil {
			t.Fatal(err)
		}
		fault := &importFaultDriver{at: point}
		err := withFaultyConn(t, f.db, false, fault, func() error { return f.db.DeleteProfile(ctx, f.profile) })
		if _, getErr := f.db.GetProfile(ctx, f.profile); getErr != nil {
			t.Fatalf("read failure %d (fired=%t, err=%v) deleted the movie default profile", point, fault.fired, err)
		}
		if !fault.fired {
			if !errors.Is(err, ErrProfileIsDefault) {
				t.Fatalf("unfaulted delete of a default = %v", err)
			}
			return
		}
		if err == nil {
			t.Fatalf("read failure %d returned success", point)
		}
	}
	t.Fatal("delete never completed without an injected failure")
}

func TestAcquisitionStoreReadFailuresNeverBecomeEmptySuccess(t *testing.T) {
	ctx := context.Background()
	f := newAcquisitionStoreFixture(t)
	db := f.db
	calls := map[string]func() (int, error){
		"profiles":       func() (int, error) { v, e := db.ListProfiles(ctx); return len(v), e },
		"indexers":       func() (int, error) { v, e := db.ListIndexers(ctx); return len(v), e },
		"with-payload":   func() (int, error) { v, e := db.ImportedWithPayload(ctx, 10); return len(v), e },
		"active":         func() (int, error) { v, e := db.ListActiveDownloads(ctx); return len(v) + 1, e },
		"recent":         func() (int, error) { v, e := db.ListRecentDownloads(ctx); return len(v), e },
		"retry":          func() (int, error) { v, e := db.ListImportRetryCandidates(ctx); return len(v), e },
		"file-qualities": func() (int, error) { v, e := db.FileQualities(ctx, f.item); return len(v), e },
		"history":        func() (int, error) { v, e := db.ListHistory(ctx); return len(v), e },
		"queue-page":     func() (int, error) { v, e := db.QueuePage(ctx, QueueFailed, "Store", 10, 0); return len(v), e },
		"queue-counts":   func() (int, error) { v, e := db.QueueCounts(ctx); return len(v), e },
		"history-page":   func() (int, error) { v, e := db.HistoryPage(ctx, f.item, 10, 0); return len(v), e },
	}
	before := pipelineSnapshot(t, db)
	for name, call := range calls {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%t", name, malformed), func(t *testing.T) {
				for point := 1; point <= 32; point++ {
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
						return
					}
					if err == nil {
						t.Fatalf("read failure %d returned success with %d rows", point, count)
					}
				}
				t.Fatal("read never completed without injection")
			})
		}
	}
	if after := pipelineSnapshot(t, db); after != before {
		t.Fatal("a read changed durable state")
	}
}
