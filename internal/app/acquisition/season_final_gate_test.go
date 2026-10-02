package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pjunod/monarr/internal/adapters/torznab"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestRSSQueuesEvidenceWithoutDispatchAndSuppressesInferiorRepeats(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	w, err := s.wantableFromID(ctx, fmt.Sprintf("episode:%d:1:1", item))
	if err != nil {
		t.Fatal(err)
	}
	ep := w.(domain.EpisodeWantable)
	profile, _ := db.GetProfile(ctx, ep.ProfileID())
	good := ports.Release{Title: "Test.Show.S01E01.1080p.WEB-DL-GOOD", DownloadURL: "good", Indexer: "idx", Protocol: "torrent", Seeders: 10, Size: 1000}
	if err = s.enqueueRSSSeason(ctx, ep, good, profile); err != nil {
		t.Fatal(err)
	}
	lower := good
	lower.Title = "Test.Show.S01E01.720p.HDTV-LOW"
	lower.DownloadURL = "low"
	if err = s.enqueueRSSSeason(ctx, ep, lower, profile); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = db.R.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE kind=?`, SeasonSearchJobKind).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var checkpoint seasonCheckpoint
	_ = json.Unmarshal([]byte(raw), &checkpoint)
	if len(checkpoint.Releases) != 1 || len(client.added) != 0 {
		t.Fatalf("feed dispatched or merged inferior evidence: %+v", checkpoint)
	}
	// A distinct higher-quality episode remains useful supply evidence.
	better := good
	better.Title = "Test.Show.S01E02.1080p.BluRay-BETTER"
	better.DownloadURL = "better"
	second, _ := s.wantableFromID(ctx, fmt.Sprintf("episode:%d:1:2", item))
	if err = s.enqueueRSSSeason(ctx, second.(domain.EpisodeWantable), better, profile); err != nil {
		t.Fatal(err)
	}
	runQueuedSeason(t, s, db)
	if len(client.added) != 2 {
		t.Fatalf("complete comparison did not serve both episodes: %v", client.added)
	}
}

func TestPartialCleanupRetainsReferencedBytesAndRemovesOnlyOwnedUnreferencedPayload(t *testing.T) {
	for _, mode := range []string{"safe", "absent", "outside", "referenced"} {
		t.Run(mode, func(t *testing.T) {
			s, db, item, root := completedFixture(t)
			ctx := context.Background()
			path := filepath.Join(root, "partial")
			if mode == "outside" {
				path = filepath.Join(t.TempDir(), "partial")
			}
			if mode != "absent" {
				writeCompleted(t, filepath.Join(path, "file.mkv"), 12)
			}
			id, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "failed", Protocol: "torrent", ReleaseTitle: "partial", ImportPath: path, SavePath: path})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET cleanup_pending=1,import_path=?,save_path=? WHERE id=?`, path, path, id); err != nil {
				t.Fatal(err)
			}
			if mode == "referenced" {
				if _, err = db.UpsertFile(ctx, item, 0, filepath.Join(path, "file.mkv"), 12); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ScanCompleted(ctx); err != nil {
				t.Fatal(err)
			}
			s.cleanupPartialPayloads(ctx)
			got, _ := db.GetDownload(ctx, id)
			protected := mode == "outside" || mode == "referenced"
			if got.CleanupPending != protected {
				inv, _ := s.CompletedInventory(ctx)
				t.Fatalf("cleanup custody %s: pending=%v inventory=%+v", mode, got.CleanupPending, inv)
			}
			_, err = os.Stat(path)
			if protected && err != nil {
				t.Fatal("protected payload removed", err)
			}
			if !protected && !os.IsNotExist(err) {
				t.Fatal("owned payload survived", err)
			}
		})
	}
}

func TestManualMultipartSidegradePreservesEveryOriginalByte(t *testing.T) {
	s, db, item := bookSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	_, err := db.W.ExecContext(ctx, `UPDATE media_items SET book_type='audiobook',quality_profile_id=? WHERE id=?`, quality.AudiobookProfileID, item)
	if err != nil {
		t.Fatal(err)
	}
	payload := func(prefix string) string {
		dir := t.TempDir()
		for _, name := range []string{"01.mp3", "02.mp3"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(prefix+name), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	dl := sqlite.Download{MediaItemID: item, ReleaseTitle: "Project Hail Mary MP3"}
	if _, err = s.importDownload(ctx, dl, payload("original-"), false); err != nil {
		t.Fatal(err)
	}
	original, _ := db.ListFilesForItem(ctx, item)
	res, err := s.importDownload(ctx, dl, payload("manual-"), true)
	if err != nil || res.Imported != 2 || res.Upgraded {
		t.Fatalf("manual import %+v %v", res, err)
	}
	for _, f := range original {
		b, err := os.ReadFile(f.Path)
		if err != nil || string(b[:9]) != "original-" {
			t.Fatal("original bytes replaced", f.Path, err)
		}
	}
	files, _ := db.ListFilesForItem(ctx, item)
	if len(files) != 4 {
		t.Fatalf("manual sidegrade lost a track: %+v", files)
	}
}

func TestDiscoveryInterruptedEpochRefreshesWithoutDispatch(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, "test", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var p seasonCheckpoint
	_ = json.Unmarshal([]byte(j.Payload), &p)
	p.Started = time.Now().Add(-25 * time.Hour)
	p.Deadline = p.Started.Add(24 * time.Hour)
	p.Revision, _ = db.AcquisitionRevision(ctx)
	raw, _ := json.Marshal(p)
	j.Payload = string(raw)
	if err = s.handleSeasonSearch(ctx, j); err == nil {
		t.Fatal("expired epoch dispatched")
	}
	fresh, _ := db.GetJob(ctx, j.ID)
	_ = json.Unmarshal([]byte(fresh.Payload), &p)
	if !p.Started.IsZero() || !p.SchedulingDeferred || len(p.Releases) != 0 {
		t.Fatalf("stale evidence retained: %+v", p)
	}
}

type retiringClient struct{ fakeClient }

func (c *retiringClient) Remove(ctx context.Context, h ports.Handle, deleteData bool) error {
	if deleteData {
		return fmt.Errorf("partial bytes must survive retirement")
	}
	if err := c.fakeClient.Remove(ctx, h, false); err != nil {
		return err
	}
	c.mu.Lock()
	c.statuses = nil
	c.mu.Unlock()
	return nil
}
func TestConfirmedStallRetirementChargesFailureAndPreservesPartialCustody(t *testing.T) {
	client := &retiringClient{}
	s, db, item := setup(t, nil, &client.fakeClient)
	ctx := context.Background()
	s.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	_, rows := admittedFixture(t, s, db, item, "single")
	dl := rows[0]
	_, _ = db.ClaimPlannedSubmission(ctx, dl.ID)
	_ = db.SetSubmissionPhase(ctx, dl.ID, "submitted")
	_ = db.SetDownloadHandle(ctx, dl.ID, "h1", dl.Transfer)
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)
	bytes := int64(100)
	zero := 0
	st := ports.DownloadStatus{Handle: "h1", Name: dl.ReleaseTitle, State: ports.StateDownloading, OperationalState: "stalledDL", BytesCompleted: &bytes, ConnectedSeeds: &zero, SavePath: filepath.Join(t.TempDir(), "partial")}
	client.statuses = []ports.DownloadStatus{st}
	_ = db.UpdateDownloadObservation(ctx, dl.ID, executionObservation{BytesKnown: true, LastBytes: bytes, ShortStall: 48 * time.Hour, LastStallObservation: time.Now().Add(-time.Minute)})
	dl, _ = db.GetDownload(ctx, dl.ID)
	s.observeTorrentStall(ctx, dl, cfg, st)
	fresh, _ := db.GetDownload(ctx, dl.ID)
	if fresh.State != "failed" || !fresh.CleanupPending || len(client.removed) != 1 {
		t.Fatalf("confirmed retirement %+v", fresh)
	}
}

func TestDispatchRejectsConfigurationChangesBeforeAdd(t *testing.T) {
	for _, mode := range []string{"disabled", "credentials", "profile"} {
		t.Run(mode, func(t *testing.T) {
			client := &fakeClient{}
			s, db, item := setup(t, nil, client)
			ctx := context.Background()
			id, rows := admittedFixture(t, s, db, item, "single")
			switch mode {
			case "disabled":
				_, _ = db.W.ExecContext(ctx, `UPDATE download_clients SET enabled=0 WHERE id=?`, rows[0].ClientID)
			case "credentials":
				_, _ = db.W.ExecContext(ctx, `UPDATE download_clients SET password='changed' WHERE id=?`, rows[0].ClientID)
			case "profile":
				_, _ = db.W.ExecContext(ctx, `UPDATE media_items SET quality_profile_id=3 WHERE id=?`, item)
			}
			if err := s.dispatchPlan(ctx, id); err != nil {
				t.Fatal(err)
			}
			p, _ := db.GetAcquisitionPlan(ctx, id)
			if p.State != "needs_replan" || len(client.added) != 0 {
				t.Fatalf("changed %s submitted: %+v %v", mode, p, client.added)
			}
		})
	}
}

func TestRealTorznabAutomaticDiscoveryDebitsOnlyWireRequests(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(`<caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`))
			return
		}
		_, _ = w.Write([]byte(`<rss><channel><item><title>Test.Show.S01E01.1080p.WEB-DL-ONE</title><link>one</link><size>1000</size></item><item><title>Test.Show.S01E02.1080p.WEB-DL-TWO</title><link>two</link><size>1000</size></item></channel></rss>`))
	}))
	defer remote.Close()
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	configs, _ := db.ListIndexers(ctx)
	cfg := configs[0]
	cfg.URL = remote.URL
	cfg.APIKey = "fixture"
	s.newIndexer = func(in ports.IndexerConfig) ports.Indexer {
		in.URL = remote.URL
		in.APIKey = "fixture"
		return torznab.New(in)
	}
	if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
		t.Fatal(err)
	}
	runQueuedSeason(t, s, db)
	if len(client.added) != 2 {
		t.Fatalf("strict automatic discovery failed: %v", client.added)
	}
	var charged int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM indexer_request_usage WHERE indexer_id=?`, cfg.ID).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if charged != int(calls.Load()) || charged == 0 {
		t.Fatalf("wire=%d ledger=%d", calls.Load(), charged)
	}
}

func TestCleanupSeesPlacementReferenceBeyondRecoveryBatch(t *testing.T) {
	s, db, item, root := completedFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "partial")
	writeCompleted(t, filepath.Join(path, "file.mkv"), 12)
	id, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "failed", Protocol: "torrent", ReleaseTitle: "partial"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.W.ExecContext(ctx, `UPDATE downloads SET cleanup_pending=1,import_path=? WHERE id=?`, path, id)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 101; n++ {
		source := fmt.Sprintf("/unrelated/%d", n)
		if n == 100 {
			source = path
		}
		if err = db.PreparePlacement(ctx, sqlite.Placement{ID: fmt.Sprintf("pending-%03d", n), ItemID: item, Source: source, Target: fmt.Sprintf("/target/%d", n), State: "prepared"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	s.cleanupPartialPayloads(ctx)
	dl, _ := db.GetDownload(ctx, id)
	if !dl.CleanupPending {
		t.Fatal("101st unresolved reference lost custody")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("referenced bytes deleted", err)
	}
}
