package acquisition

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Grab admission, operator removal, and a legacy client's removed and failed
// observations, reduced the way a Runner terminal is (contract section 3.3).

func episodeGrab(item int64, title string) GrabRequest {
	return GrabRequest{MediaItemID: item, Season: 1, Episode: 1, Title: title,
		DownloadURL: "http://dl/" + title, Indexer: "idx", Protocol: "torrent", Size: 1000}
}

// What refuses a grab before the client is asked.
func TestGrabRefusals(t *testing.T) {
	t.Run("an item with no library folder", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, nil, client)
		ctx := context.Background()
		if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET path='' WHERE id=?`, item); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Grab(ctx, episodeGrab(item, epOne)); !errors.Is(err, ErrNoLibraryFolder) {
			t.Fatalf("err = %v, want no library folder", err)
		}
		if len(client.added) != 0 {
			t.Fatal("the client was asked before the folder was checked")
		}
	})
	t.Run("a protocol with no enabled client", func(t *testing.T) {
		svc, _, item := setup(t, nil, &fakeClient{})
		req := episodeGrab(item, epOne)
		req.Protocol = "usenet"
		if _, err := svc.Grab(context.Background(), req); !errors.Is(err, ErrNoClient) {
			t.Fatalf("err = %v, want no client", err)
		}
	})
	t.Run("an expired search token", func(t *testing.T) {
		svc, _, item := setup(t, nil, &fakeClient{})
		req := episodeGrab(item, epOne)
		req.CandidateToken = "not-issued"
		if _, err := svc.Grab(context.Background(), req); !errors.Is(err, ErrCandidateExpired) {
			t.Fatalf("err = %v, want expired evidence", err)
		}
	})
	t.Run("a blocklisted title needs a manual override", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, nil, client)
		ctx := context.Background()
		if err := db.AddBlocklist(ctx, item, epOne, "idx", ""); err != nil {
			t.Fatal(err)
		}
		_, err := svc.Grab(ctx, episodeGrab(item, epOne))
		if !errors.Is(err, ErrBlacklistConfirmationRequired) || !strings.Contains(err.Error(), "previously failed") {
			t.Fatalf("err = %v, want the override required with a default reason", err)
		}
	})
	t.Run("a blocklisted release identity names its own reason", func(t *testing.T) {
		svc, db, item := setup(t, nil, &fakeClient{})
		ctx := context.Background()
		var indexerID int64
		if err := db.R.QueryRowContext(ctx, `SELECT MIN(id) FROM indexers`).Scan(&indexerID); err != nil {
			t.Fatal(err)
		}
		if err := db.AddReleaseIdentityBlocklist(ctx, item, indexerID, "guid-1", "fake release"); err != nil {
			t.Fatal(err)
		}
		req := episodeGrab(item, epOne)
		req.SourceGUID, req.IndexerID = "guid-1", indexerID
		_, err := svc.Grab(ctx, req)
		if !errors.Is(err, ErrBlacklistConfirmationRequired) || !strings.Contains(err.Error(), "fake release") {
			t.Fatalf("err = %v, want the identity's reason", err)
		}
	})
}

// A grab carrying a search token keeps the evidence the search judged: a
// match as it was, a rejection as an explicit manual override.
func TestGrabKeepsItsSearchEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		matched bool
		method  string
	}{
		{"matched", true, "title"},
		{"rejected then grabbed", false, "manual_override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db, item := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			req := episodeGrab(item, epOne)
			release := ports.Release{Title: req.Title, DownloadURL: req.DownloadURL, Indexer: req.Indexer, GUID: "g-7", IndexerID: 3}
			evidence := domain.MatchEvidence{Version: 1, Method: "title", Matched: tc.matched, Reason: "different show"}
			req.CandidateToken = svc.issueCandidateToken(item, 0, 1, 1, release, evidence)
			id, err := svc.Grab(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			dl, err := db.GetDownload(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if dl.MatchEvidence.Method != tc.method || dl.SourceGUID != "g-7" || dl.SourceIndexerID != 3 {
				t.Fatalf("evidence %+v guid %q indexer %d, want %s with the token's identity", dl.MatchEvidence, dl.SourceGUID, dl.SourceIndexerID, tc.method)
			}
			if !tc.matched && !strings.Contains(dl.MatchEvidence.Reason, "different show") {
				t.Fatalf("override reason = %q, want the search's rejection kept", dl.MatchEvidence.Reason)
			}
		})
	}
}

// A replacement operation cannot adopt another acquisition already in
// flight for the same release.
func TestReplacementGrabCannotAdoptAnotherAcquisition(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	if _, _, err := f.db.CreateReplacementOperation(f.ctx, sqlite.ReplacementOperation{ID: "op-adopt", RequestID: "op-adopt", Fingerprint: "op-adopt",
		State: "searching", Intent: "replace_quality", Scope: "download", MediaItemID: f.item, ReasonCode: "quality_preference", Reason: "better"}, nil); err != nil {
		t.Fatal(err)
	}
	req := episodeGrab(f.item, epOne)
	req.ReplacementOperationID = "op-adopt"
	if _, err := f.svc.Grab(f.ctx, req); !errors.Is(err, sqlite.ErrReplacementScopeConflict) {
		t.Fatalf("err = %v, want the scope conflict", err)
	}
	if got := f.row(dl.ID); got.ReplacementOperationID != "" {
		t.Fatal("the in-flight row was adopted by the operation")
	}
	// The same release again, outside an operation, is the row in flight.
	if id, err := f.svc.Grab(f.ctx, episodeGrab(f.item, epOne)); err != nil || id != dl.ID {
		t.Fatalf("second grab = %d (%v), want the in-flight row %d", id, err, dl.ID)
	}
}

// An operator's removal of a legacy-client row: the client must confirm it
// before the row is parked; a row the client never accepted is deleted.
func TestRemoveLegacyDownloadMatrix(t *testing.T) {
	t.Run("the client still lists it", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, nil, client)
		ctx := context.Background()
		id, err := svc.Grab(ctx, episodeGrab(item, epOne))
		if err != nil {
			t.Fatal(err)
		}
		client.statuses = []ports.DownloadStatus{{Handle: "h1", Name: epOne, State: ports.StateDownloading}}
		if err := svc.RemoveDownload(ctx, id, true); err == nil || !strings.Contains(err.Error(), "awaiting confirmation") {
			t.Fatalf("err = %v, want removal awaiting the client's confirmation", err)
		}
		if dl, _ := db.GetDownload(ctx, id); dl.ParkedReason != "" {
			t.Fatal("the row was parked before the client confirmed")
		}
	})
	t.Run("the client refuses", func(t *testing.T) {
		client := &fakeClient{removeErr: errors.New("permission denied")}
		svc, _, item := setup(t, nil, client)
		ctx := context.Background()
		id, _ := svc.Grab(ctx, episodeGrab(item, epOne))
		if err := svc.RemoveDownload(ctx, id, true); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("err = %v, want the client's refusal", err)
		}
	})
	t.Run("the client confirms", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, nil, client)
		ctx := context.Background()
		id, _ := svc.Grab(ctx, episodeGrab(item, epOne))
		if err := svc.RemoveDownload(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		dl, _ := db.GetDownload(ctx, id)
		if !strings.HasPrefix(dl.ParkedReason, "operator cancellation") || len(client.removed) != 1 || client.removed[0].DeleteData {
			t.Fatalf("parked %q removes %+v, want parked for review with the data kept", dl.ParkedReason, client.removed)
		}
		var dismissed int
		_ = db.R.QueryRowContext(ctx, `SELECT activity_dismissed FROM downloads WHERE id=?`, id).Scan(&dismissed)
		if dismissed != 1 {
			t.Fatal("the removed row is still in Activity")
		}
	})
	t.Run("an unsubmitted intent is cancelled in place", func(t *testing.T) {
		svc, db, item := setup(t, nil, &fakeClient{})
		ctx := context.Background()
		id, _ := svc.Grab(ctx, episodeGrab(item, epOne))
		if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='pending' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if err := svc.RemoveDownload(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		dl, _ := db.GetDownload(ctx, id)
		if dl.State != "failed" || dl.SubmissionPhase != "rejected" || !dl.Superseded || len(dl.WantableIDs) != 0 {
			t.Fatalf("row = %s/%s superseded=%v wants=%v, want a cancelled intent", dl.State, dl.SubmissionPhase, dl.Superseded, dl.WantableIDs)
		}
	})
	t.Run("a refused submission's row is deleted", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, nil, client)
		ctx := context.Background()
		id, _ := svc.Grab(ctx, episodeGrab(item, epOne))
		if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='rejected', plan_id=NULL, state='failed' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if err := svc.RemoveDownload(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.GetDownload(ctx, id); !errors.Is(err, sqlite.ErrNotFound) {
			t.Fatalf("row lookup = %v, want deleted", err)
		}
		if len(client.removed) != 1 {
			t.Fatal("the client was not asked to remove its item")
		}
	})
}

// An operator's cancel of a Runner row: a Runner refusal leaves the row
// alone; a row Runner already finished is only dismissed.
func TestCancelRunnerDownloadMatrix(t *testing.T) {
	t.Run("Runner refuses the cancel", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		f.runner.removeErr = errors.New("runner unreachable")
		if err := f.svc.RemoveDownload(f.ctx, dl.ID, true); err == nil {
			t.Fatal("a refused Runner cancel succeeded")
		}
		if got := f.row(dl.ID); got.RunnerOutcome != "" || got.State != "downloading" {
			t.Fatalf("row = %s/%s, want untouched", got.State, got.RunnerOutcome)
		}
	})
	t.Run("a finished row is dismissed, not cancelled", func(t *testing.T) {
		f := setupV2(t, nil)
		f.deferGrabs()
		dl := f.grab(1, epOne)
		f.observe(dl, terminalFact(dl, failed(ports.ReasonHealth, true), "event 1"))
		if err := f.svc.RemoveDownload(f.ctx, dl.ID, true); err != nil {
			t.Fatal(err)
		}
		got := f.row(dl.ID)
		f.runner.mu.Lock()
		removes := len(f.runner.removed)
		f.runner.mu.Unlock()
		if got.RunnerOutcome != "failed" || removes != 0 {
			t.Fatalf("outcome %q removes %d, want the failure kept and Runner not asked", got.RunnerOutcome, removes)
		}
		if f.count(`SELECT activity_dismissed FROM downloads WHERE id=?`, dl.ID) != 1 {
			t.Fatal("the row was not dismissed")
		}
	})
	t.Run("removing from Curator only leaves the Runner job", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		if err := f.svc.RemoveDownload(f.ctx, dl.ID, false); err != nil {
			t.Fatal(err)
		}
		f.runner.mu.Lock()
		removes := len(f.runner.removed)
		f.runner.mu.Unlock()
		if got := f.row(dl.ID); got.RunnerOutcome != ports.OutcomeCancelled || removes != 0 {
			t.Fatalf("outcome %q removes %d, want cancelled here with Runner left alone", got.RunnerOutcome, removes)
		}
	})
}

// A legacy client's removed and failed observations become terminals with
// the same rules a Runner terminal has.
func TestLegacyClientObservationsAreTerminals(t *testing.T) {
	t.Run("removed in the client is cancelled, never replaced", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)}, client)
		ctx := context.Background()
		id, _ := svc.Grab(ctx, episodeGrab(item, epOne))
		client.statuses = []ports.DownloadStatus{{Handle: "h1", Name: epOne, State: ports.StateRemoved, Progress: 0.4}}
		if err := svc.RefreshQueue(ctx); err != nil {
			t.Fatal(err)
		}
		dl, _ := db.GetDownload(ctx, id)
		if dl.RunnerOutcome != ports.OutcomeCancelled || dl.Error != "removed in the download client" {
			t.Fatalf("row = %s %q, want cancelled with the default reason", dl.RunnerOutcome, dl.Error)
		}
		if bl, _ := db.ListBlocklist(ctx); len(bl) != 0 {
			t.Fatal("a removal blocklisted the release")
		}
		var grabs int
		_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_grabs`).Scan(&grabs)
		if grabs != 0 {
			t.Fatal("a removal wrote a replacement grab")
		}
	})
	t.Run("a refused submission's failure replaces nothing", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)}, client)
		ctx := context.Background()
		id, _ := svc.Grab(ctx, episodeGrab(item, epOne))
		dl, _ := db.GetDownload(ctx, id)
		if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='rejected' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		dl.SubmissionPhase = "rejected"
		svc.handleFailure(ctx, dl, 0.5, "client refused the torrent", false)
		got, _ := db.GetDownload(ctx, id)
		if got.RunnerOutcome != "failed" || got.Error != "client refused the torrent" || got.Progress != 0.5 {
			t.Fatalf("row = %s %q progress %.2f, want the client's own words and measure", got.RunnerOutcome, got.Error, got.Progress)
		}
		var grabs int
		_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_grabs`).Scan(&grabs)
		if bl, _ := db.ListBlocklist(ctx); len(bl) != 0 || grabs != 0 {
			t.Fatalf("blocklist %v grabs %d, want neither for a refused submission", bl, grabs)
		}
	})
	t.Run("a blameless failure is not blocklisted", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, item := setup(t, nil, client)
		ctx := context.Background()
		id, _ := svc.Grab(ctx, episodeGrab(item, epOne))
		svc.grabWorker.Store(true)
		dl, _ := db.GetDownload(ctx, id)
		svc.handleFailure(ctx, dl, 1, "disk full", true)
		var grabs int
		_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_grabs`).Scan(&grabs)
		if bl, _ := db.ListBlocklist(ctx); len(bl) != 0 || grabs != 1 {
			t.Fatalf("blocklist %v grabs %d, want no blocklist and one replacement", bl, grabs)
		}
	})
}

// Wantable ids that do not name a wantable of this library.
func TestWantableFromIDRefusals(t *testing.T) {
	svc, _, item := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	for _, id := range []string{"garbage", "movie:" + strconv.FormatInt(item, 10), "book:" + strconv.FormatInt(item, 10), "episode:" + strconv.FormatInt(item, 10) + ":1:1:c999", "season:99999:1"} {
		if _, err := svc.wantableFromID(ctx, id); err == nil {
			t.Errorf("%s: accepted", id)
		}
	}
	if w, err := svc.wantableFromID(ctx, "season:"+strconv.FormatInt(item, 10)+":1"); err != nil || w == nil {
		t.Fatalf("season wantable = %v (%v)", w, err)
	}
}

// How a status finds its row: exact token for negotiated rows (a different
// job id under the same token is not a match), handle, then a unique name.
func TestMatchStatusRules(t *testing.T) {
	exact := sqlite.Download{Transfer: "t-1", Handle: "7", ExactTransferCorrelation: true}
	if _, ok := matchStatus(exact, []ports.DownloadStatus{{Transfer: "t-1", Handle: "8"}}); ok {
		t.Error("a token under another job id matched")
	}
	if _, ok := matchStatus(exact, []ports.DownloadStatus{{Transfer: "t-1", Handle: "7"}, {Transfer: "t-1", Handle: "7"}}); ok {
		t.Error("an ambiguous token matched")
	}
	manual := sqlite.Download{Transfer: "t-2", Handle: "9", Intent: GrabIntentManualAdditional}
	if st, ok := matchStatus(manual, []ports.DownloadStatus{{Handle: "9"}, {Transfer: "t-other", Handle: "10"}}); !ok || st.Handle != "9" {
		t.Error("a tokenless status did not match a manual row by its handle")
	}
	if _, ok := matchStatus(manual, []ports.DownloadStatus{{Transfer: "t-2", Handle: "11"}}); ok {
		t.Error("a manual row matched its token under another job id")
	}
	byName := sqlite.Download{ReleaseTitle: "Some.Show-GRP"}
	if _, ok := matchStatus(byName, []ports.DownloadStatus{{Name: "some show grp"}, {Name: "Some_Show.GRP"}}); ok {
		t.Error("an ambiguous name matched")
	}
	if st, ok := matchStatus(byName, []ports.DownloadStatus{{Name: "some show grp", Handle: "x"}}); !ok || st.Handle != "x" {
		t.Error("a unique normalized name did not match")
	}
}
