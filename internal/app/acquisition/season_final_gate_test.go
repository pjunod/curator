package acquisition

import (
	"context"
	"encoding/json"
	"errors"
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

type interruptedIndexer struct {
	fakeIndexer
	capabilityErr error
	searchErr     error
}

func (i *interruptedIndexer) Capabilities(context.Context) (ports.IndexerCapabilities, error) {
	return ports.IndexerCapabilities{}, i.capabilityErr
}
func (i *interruptedIndexer) Search(ctx context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	if i.searchErr != nil {
		return nil, i.searchErr
	}
	return i.fakeIndexer.Search(ctx, q)
}
func TestDiscoveryRetriesRetainScopeAndRecoverWithoutIncompleteDispatch(t *testing.T) {
	for _, mode := range []string{"capability", "search", "provider_retry"} {
		t.Run(mode, func(t *testing.T) {
			client := &fakeClient{}
			s, db, item := setup(t, nil, client)
			ctx := context.Background()
			indexer := &interruptedIndexer{fakeIndexer: fakeIndexer{releases: []ports.Release{
				{Title: "Test.Show.S01E01.1080p.WEB-DL-ONE", DownloadURL: "one", Indexer: "idx", Protocol: "torrent", Size: 1000},
				{Title: "Test.Show.S01E02.1080p.WEB-DL-TWO", DownloadURL: "two", Indexer: "idx", Protocol: "torrent", Size: 1000},
			}}}
			if mode == "capability" {
				indexer.capabilityErr = errors.New("temporary capability outage")
			} else {
				indexer.searchErr = errors.New("temporary search outage")
			}
			if mode == "provider_retry" {
				indexer.searchErr = &ports.RemoteError{Category: ports.RemoteRateLimit, RetryAt: time.Now().Add(time.Hour)}
			}
			s.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
			if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
				t.Fatal(err)
			}
			j, err := db.ClaimJob(ctx, "retry", nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.handleSeasonSearch(ctx, j); err == nil || len(client.added) != 0 {
				t.Fatal("incomplete scope dispatched", err)
			}
			fresh, _ := db.GetJob(ctx, j.ID)
			var p seasonCheckpoint
			_ = json.Unmarshal([]byte(fresh.Payload), &p)
			if p.Started.IsZero() || len(p.Scopes) != 1 || p.Scopes[0].Complete {
				t.Fatalf("retry lost scope: %+v", p)
			}
			// Move the disposable fixture to the provider's retry epoch, without sleeping.
			p.Scopes[0].RetryAt = time.Time{}
			raw, _ := json.Marshal(p)
			j.Payload = string(raw)
			_, _ = db.W.ExecContext(ctx, `UPDATE indexers SET retry_at=0`)
			indexer.capabilityErr = nil
			indexer.searchErr = nil
			if err = s.handleSeasonSearch(ctx, j); err != nil {
				t.Fatal("recovered scope failed", err)
			}
			if len(client.added) != 2 {
				t.Fatalf("recovered scope did not serve targets: %v", client.added)
			}
		})
	}
}

type resumableClient struct {
	fakeClient
	resumed ports.Handle
}

func (c *resumableClient) Resume(_ context.Context, h ports.Handle) error { c.resumed = h; return nil }
func TestSameJobResumeRetainsHoldUntilAuthoritativeSnapshot(t *testing.T) {
	c := &resumableClient{}
	s, db, item := setup(t, nil, &c.fakeClient)
	ctx := context.Background()
	s.newClient = func(ports.ClientConfig) ports.DownloadClient { return c }
	id, err := s.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL", DownloadURL: "one", Protocol: "torrent"})
	if err != nil {
		t.Fatal(err)
	}
	control := `{"version":1,"lifecycle":"held","retry_policy":"resume_same_job","revision":"one"}`
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET runner_control=? WHERE id=?`, control, id); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeHeldDownload(ctx, id); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	if c.resumed != "h1" || dl.RunnerControl != control || len(c.added) != 1 {
		t.Fatalf("resume lost original custody: %+v", dl)
	}
}

func TestFailedSingleFallbackCannotStealAnotherReservation(t *testing.T) {
	for _, scope := range []string{"season", "episode"} {
		t.Run(scope, func(t *testing.T) {
			s, db, item := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			_, rows := admittedFixture(t, s, db, item, "pack", "single")
			pack, single := rows[0], rows[1]
			_, _ = db.ClaimPlannedSubmission(ctx, single.ID)
			_ = db.SetSubmissionPhase(ctx, single.ID, "submitted")
			_ = db.UpdateDownloadState(ctx, single.ID, "failed", 0, "client failure")
			want := fmt.Sprintf("season:%d:1", item)
			if scope == "episode" {
				want = fmt.Sprintf("episode:%d:1:2", item)
			}
			_, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, Season: 1, State: "grabbed", Protocol: "torrent", ReleaseTitle: "manual replacement", WantableIDs: []string{want}})
			if err != nil {
				t.Fatal(err)
			}
			allowed, err := s.effectiveImportAllowlist(ctx, pack)
			if err != nil || len(allowed) != 1 {
				t.Fatalf("pack stole %s reservation: %v %v", scope, allowed, err)
			}
		})
	}
}

func TestThreeProviderFailuresEndEpochWithoutClaimingAbsence(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	indexer := &interruptedIndexer{searchErr: errors.New("provider unavailable")}
	s.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
	if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, "retry", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err = s.handleSeasonSearch(ctx, j); err == nil {
			t.Fatal("failed provider asserted completion")
		}
		fresh, _ := db.GetJob(ctx, j.ID)
		var p seasonCheckpoint
		_ = json.Unmarshal([]byte(fresh.Payload), &p)
		if attempt < 2 {
			if len(p.Scopes) != 1 || p.Scopes[0].Failures != attempt+1 {
				t.Fatalf("failure evidence lost %+v", p)
			}
			p.Scopes[0].RetryAt = time.Time{}
			raw, _ := json.Marshal(p)
			j.Payload = string(raw)
		} else if !p.Started.IsZero() || !p.CooldownUntil.IsZero() {
			t.Fatalf("incomplete evidence became absence cooldown: %+v", p)
		}
	}
}

func TestUnplannedAcknowledgmentFencesStaleQueuedImportAndKeepsStandaloneManualInputs(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id, err := s.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL", DownloadURL: "one", Protocol: "torrent"})
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := db.GetDownload(ctx, id)
	_ = db.SetSubmissionPhase(ctx, id, "uncertain")
	if err = s.ResolveReservation(ctx, id); err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	if err = os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv"), []byte("incoming"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.importDownload(ctx, stale, payload, false); err == nil {
		t.Fatal("stale queued original published after acknowledgment")
	}
	rows, _ := db.ListFilesForItem(ctx, item)
	if len(rows) != 0 {
		t.Fatal("late original reached library")
	}
	var jobs int
	_ = db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=?`, SeasonSearchJobKind).Scan(&jobs)
	if jobs != 0 {
		t.Fatal("operator acknowledgment automatically acquired a replacement")
	}
	if _, err = s.importDownload(ctx, sqlite.Download{MediaItemID: item, ReleaseTitle: "standalone manual"}, payload, true); err != nil {
		t.Fatal("standalone manual import was gated", err)
	}
}

func TestAdvertisedPagesBeyondBoundCannotAuthorizePack(t *testing.T) {
	var pages atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "caps" {
			_, _ = w.Write([]byte(`<caps><searching><search available="yes" supportedParams="q"/><tv-search available="yes" supportedParams="q,season,ep"/></searching></caps>`))
			return
		}
		pages.Add(1)
		offset := r.URL.Query().Get("offset")
		if offset == "" {
			offset = "0"
		}
		_, _ = fmt.Fprintf(w, `<rss xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/"><channel><newznab:response offset="%s" total="200"/><item><title>Test.Show.S01.1080p.WEB-DL-PACK</title><link>pack</link><size>1000</size></item></channel></rss>`, offset)
	}))
	defer remote.Close()
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	s.newIndexer = func(in ports.IndexerConfig) ports.Indexer {
		in.URL = remote.URL
		in.APIKey = "fixture"
		return torznab.New(in)
	}
	if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, "bounded", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.handleSeasonSearch(ctx, j); err == nil {
		t.Fatal("incomplete pages became complete absence")
	}
	if pages.Load() < 2 || pages.Load() > 18 || len(client.added) != 0 {
		t.Fatalf("pagination/pack bound: calls=%d adds=%v", pages.Load(), client.added)
	}
}

func TestDiscoveryUnavailableCopyAndNonSeasonScopesDoNotEnqueue(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	for _, p := range []seasonCheckpoint{{Version: 1, ItemID: item, CopyID: 999, Season: 1}, {Version: 1, ItemID: 99999, Season: 1}, {Version: 1, ItemID: item, Season: 999}} {
		if _, _, _, _, _, err := s.seasonSnapshot(ctx, p); err == nil {
			t.Fatalf("unavailable scope accepted: %+v", p)
		}
	}
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{MediaItemID: item, Name: "unmonitored", QualityProfileID: 1, Path: t.TempDir(), Monitored: false})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.enqueueSeason(ctx, item, copyID, 1, "test", true); err == nil {
		t.Fatal("unmonitored copy queued")
	}
	movieS, _, movie := autoSetup(t, nil, &fakeClient{})
	if _, _, _, _, _, err = movieS.seasonSnapshot(ctx, seasonCheckpoint{Version: 1, ItemID: movie, Season: 1}); err == nil {
		t.Fatal("movie entered season discovery")
	}
}

func TestUnpublishedPlacementRestartKeepsNewerDestinationAndCommittedCleanup(t *testing.T) {
	s, db, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.mkv")
	if err := os.WriteFile(target, []byte("newer"), 0600); err != nil {
		t.Fatal(err)
	}
	prior := sqlite.Placement{ID: "older-intent", ItemID: item, Source: "unavailable", Target: target, SHA256: "different", State: "prepared"}
	if err := db.PreparePlacement(ctx, prior); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup")
	if err := os.WriteFile(backup, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, _, err := fileDigest(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	committed := sqlite.Placement{ID: "cleanup-intent", ItemID: item, Target: filepath.Join(dir, "other.mkv"), Backup: backup, PreviousSHA256: digest, State: "committed"}
	if err = db.PreparePlacement(ctx, committed); err != nil {
		t.Fatal(err)
	}
	s.reconcilePlacements(ctx)
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "newer" {
		t.Fatal("restart overwrote newer destination", err)
	}
	pending, _ := db.GetPlacement(ctx, prior.ID)
	var cleanedState string
	_ = db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, committed.ID).Scan(&cleanedState)
	if pending.State != "prepared" || cleanedState != "cleaned" {
		t.Fatalf("restart custody states: %s / %s", pending.State, cleanedState)
	}
	if _, err = os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("committed rollback bytes not cleaned", err)
	}
}

func TestDiscoveryRejectsInvalidAndUnavailableInputsBeforeTransport(t *testing.T) {
	for _, mode := range []string{"json", "version", "missing_item", "missing_path", "no_indexers"} {
		t.Run(mode, func(t *testing.T) {
			client := &fakeClient{}
			s, db, item := setup(t, nil, client)
			ctx := context.Background()
			p := seasonCheckpoint{Version: 1, ItemID: item, Season: 1}
			raw, _ := json.Marshal(p)
			switch mode {
			case "json":
				raw = []byte("bad json")
			case "version":
				p.Version = 2
				raw, _ = json.Marshal(p)
			case "missing_item":
				p.ItemID = 99999
				raw, _ = json.Marshal(p)
			case "missing_path":
				_, _ = db.W.ExecContext(ctx, `UPDATE media_items SET path='' WHERE id=?`, item)
			case "no_indexers":
				_, _ = db.W.ExecContext(ctx, `UPDATE indexers SET enabled=0`)
			}
			if err := s.handleSeasonSearch(ctx, domain.Job{Payload: string(raw)}); err == nil {
				t.Fatal("unavailable discovery accepted")
			}
			if len(client.added) != 0 {
				t.Fatal("invalid input reached client")
			}
		})
	}
}

func TestDiscoveryDefersActiveComparisonAndProviderWakeWithoutStartingEvidenceClock(t *testing.T) {
	for _, mode := range []string{"active_plan", "open_comparison", "provider_wake", "rolling_capacity"} {
		t.Run(mode, func(t *testing.T) {
			client := &fakeClient{}
			s, db, item := setup(t, nil, client)
			ctx := context.Background()
			if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
				t.Fatal(err)
			}
			j, err := db.ClaimJob(ctx, "waiting", nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			var p seasonCheckpoint
			_ = json.Unmarshal([]byte(j.Payload), &p)
			configs, _ := db.ListIndexers(ctx)
			switch mode {
			case "active_plan":
				admittedFixture(t, s, db, item, "single")
			case "open_comparison":
				_, err = db.EnqueueJob(ctx, domain.Job{Kind: SeasonSearchJobKind, DedupeKey: "other-season", Payload: `{"started":"2026-10-01T00:00:00Z"}`})
				if err != nil {
					t.Fatal(err)
				}
			case "provider_wake":
				p.Started = time.Now()
				p.Deadline = p.Started.Add(24 * time.Hour)
				p.Revision, _ = db.AcquisitionRevision(ctx)
				p.Scopes = []discoveryScope{{IndexerID: configs[0].ID, RetryAt: time.Now().Add(time.Hour)}}
				raw, _ := json.Marshal(p)
				j.Payload = string(raw)
			case "rolling_capacity":
				if err = db.SetDailyRequestCap(ctx, configs[0].ID, 50); err != nil {
					t.Fatal(err)
				}
				for range sqlite.RequestBudget(50).Search {
					if err = db.ReserveIndexerRequest(ctx, configs[0].ID, "search", false, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = s.handleSeasonSearch(ctx, j)
			var deferred *sqlite.BudgetDeferred
			if !errors.As(err, &deferred) || len(client.added) != 0 {
				t.Fatalf("unsafe wake %s: %v %v", mode, err, client.added)
			}
			if mode != "provider_wake" {
				fresh, _ := db.GetJob(ctx, j.ID)
				_ = json.Unmarshal([]byte(fresh.Payload), &p)
				if !p.Started.IsZero() {
					t.Fatal("waiting work started evidence clock")
				}
			}
		})
	}
}

func TestPolicyRevisionRefreshDiscardsPreviouslyDiscoveredSupply(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, "refresh", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var p seasonCheckpoint
	_ = json.Unmarshal([]byte(j.Payload), &p)
	p.Revision, _ = db.AcquisitionRevision(ctx)
	p.Started = time.Now().Add(-48 * time.Hour)
	p.Deadline = p.Started.Add(24 * time.Hour)
	p.Releases = []ports.Release{{Title: "Test.Show.S01.1080p.WEB-DL-STALE", DownloadURL: "stale", Indexer: "idx", Protocol: "torrent", Size: 1000}}
	p.Scopes = []discoveryScope{{IndexerID: 1, Complete: true}}
	p.Stage = "episodes"
	raw, _ := json.Marshal(p)
	j.Payload = string(raw)
	if _, err = db.W.ExecContext(ctx, `UPDATE indexers SET api_key='changed'`); err != nil {
		t.Fatal(err)
	}
	if err = s.handleSeasonSearch(ctx, j); err != nil {
		t.Fatal(err)
	}
	fresh, _ := db.GetJob(ctx, j.ID)
	_ = json.Unmarshal([]byte(fresh.Payload), &p)
	if len(p.Releases) != 0 || time.Since(p.Started) > time.Minute || len(client.added) != 0 {
		t.Fatal("policy refresh retained stale supply")
	}
}

func TestIntrinsicallyOversizedScopeServesOnlyCompletedSingles(t *testing.T) {
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	ctx := context.Background()
	for episode := 3; episode <= 20; episode++ {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO episodes(media_item_id,season_number,episode_number,title,air_date,monitored) VALUES(?,1,?,'Episode','2020-01-01',1)`, item, episode); err != nil {
			t.Fatal(err)
		}
	}
	indexers, _ := db.ListIndexers(ctx)
	cfg := indexers[0]
	if err := db.SetDailyRequestCap(ctx, cfg.ID, 50); err != nil {
		t.Fatal(err)
	}
	supply := []ports.Release{
		{Title: "Test.Show.S01E01.1080p.WEB-DL-ONE", DownloadURL: "one", Indexer: "idx", IndexerID: cfg.ID, Protocol: "torrent", Size: 1000},
		{Title: "Test.Show.S01E02.1080p.WEB-DL-TWO", DownloadURL: "two", Indexer: "idx", IndexerID: cfg.ID, Protocol: "torrent", Size: 1000},
		{Title: "Test.Show.S01.1080p.WEB-DL-PACK", DownloadURL: "pack", Indexer: "idx", IndexerID: cfg.ID, Protocol: "torrent", Size: 1000},
	}
	s.newIndexer = func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{releases: supply} }
	if err := s.enqueueSeason(ctx, item, 0, 1, "test", true); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, "oversized", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = s.handleSeasonSearch(ctx, j)
	var deferred *sqlite.BudgetDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("oversized discovery did not defer: %v", err)
	}
	fresh, _ := db.GetJob(ctx, j.ID)
	var p seasonCheckpoint
	_ = json.Unmarshal([]byte(fresh.Payload), &p)
	if !p.IntrinsicPartial || len(client.added) != 2 {
		t.Fatalf("completed singles not served: partial=%v adds=%v", p.IntrinsicPartial, client.added)
	}
	for _, url := range client.added {
		if url == "pack" {
			t.Fatal("incomplete epoch authorized pack")
		}
	}
}

type rssWakeClient struct {
	fakeClient
	called chan struct{}
}

func (c *rssWakeClient) Add(ctx context.Context, url, category string) (ports.Handle, error) {
	h, err := c.fakeClient.Add(ctx, url, category)
	close(c.called)
	return h, err
}
func TestPersistedRSSWakeDispatchesBeforeMaximumIdleSweep(t *testing.T) {
	client := &rssWakeClient{called: make(chan struct{})}
	s, db, _ := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL-WAKE", 10)}, &client.fakeClient)
	s.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := db.W.ExecContext(ctx, `UPDATE indexers SET next_rss_at=?`, time.Now().Add(20*time.Millisecond).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { s.RunRSSPacing(ctx); close(finished) }()
	select {
	case <-client.called:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("persisted due time did not wake RSS")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("RSS pacing did not stop on cancellation")
	}
	var calls int
	if err := db.R.QueryRowContext(context.Background(), `SELECT count(*) FROM indexer_request_usage WHERE bucket='rss'`).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("due sweep debit = %d %v", calls, err)
	}
}
