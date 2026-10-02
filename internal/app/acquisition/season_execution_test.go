package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/acquisitionplan"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestPlanAdmissionClaimsEpisodeAndPendingRowsAreNotClientJobs(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	ep, _ := db.GetEpisodeID(ctx, itemID, 1, 1)
	rev, _ := db.AcquisitionRevision(ctx)
	raw, _ := json.Marshal(seasonDecision{Version: 1, ImportAllowlists: map[acquisitionplan.CandidateKey][]int64{"one": {ep}}})
	intent := sqlite.Download{MediaItemID: itemID, Season: 1, ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL", Protocol: "torrent", CandidateKey: "one", WantableIDs: []string{string(mustWantable(t, svc, itemID).ID())}, ReservedEpisodes: []int64{ep}, ExecutionPayload: "{}"}
	id, err := db.AdmitAcquisitionPlan(ctx, sqlite.AcquisitionPlan{MediaItemID: itemID, Season: 1, Decision: raw, SnapshotFingerprint: "test"}, rev, []sqlite.Download{intent})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.PlanDownloads(ctx, id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	if rows[0].State != "planned" || rows[0].Transfer == "" {
		t.Fatal("intent not durable before Add")
	}
	active, _ := db.ListActiveDownloads(ctx)
	if len(active) != 0 {
		t.Fatal("planned row exposed as client job")
	}
	reservations, _ := db.ListDownloadReservations(ctx)
	if len(reservations) != 1 {
		t.Fatal("pending reservation absent")
	}
	if _, err = db.AdmitAcquisitionPlan(ctx, sqlite.AcquisitionPlan{MediaItemID: itemID, Season: 1, Decision: raw, SnapshotFingerprint: "test"}, rev, []sqlite.Download{intent}); err == nil {
		t.Fatal("second scope admitted")
	}
	a, err := db.ClaimPlannedSubmission(ctx, rows[0].ID)
	if err != nil || !a {
		t.Fatal(err)
	}
	b, err := db.ClaimPlannedSubmission(ctx, rows[0].ID)
	if err != nil || b {
		t.Fatal("double submission claim")
	}
	if err = db.DeleteDownload(ctx, rows[0].ID); err == nil {
		t.Fatal("dismiss erased uncertainty custody")
	}
}
func mustWantable(t *testing.T, s *Service, id int64) interface{ ID() domain.WantableID } {
	t.Helper()
	w, e := s.wantableFromID(context.Background(), fmt.Sprintf("episode:%d:1:1", id))
	if e != nil {
		t.Fatal(e)
	}
	return w
}
func TestAmbiguousNamesDoNotReconcile(t *testing.T) {
	if _, ok := matchStatus(sqlite.Download{ReleaseTitle: "same"}, []ports.DownloadStatus{{Name: "same", Handle: "a"}, {Name: "same", Handle: "b"}}); ok {
		t.Fatal("ambiguous name attached")
	}
}
func TestSevenDayRiskRetryIsAtomicAndPersistent(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: itemID, Season: 1, State: "grabbed", Protocol: "torrent", ReleaseTitle: "uncertain", WantableIDs: []string{fmt.Sprintf("episode:%d:1:1", itemID)}})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetSubmissionPhase(ctx, id, "uncertain"); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	ob := executionObservation{AbsenceSince: time.Now().Add(-7 * 24 * time.Hour), LastInventory: time.Now(), Inventories: 100}
	if err = svc.releaseUncertainRisk(ctx, dl, ob); err != nil {
		t.Fatal(err)
	}
	if err = svc.releaseUncertainRisk(ctx, dl, ob); err != nil {
		t.Fatal(err)
	}
	dl, _ = db.GetDownload(ctx, id)
	if !dl.Superseded || !strings.Contains(dl.Error, "Possible duplicate") {
		t.Fatal("risk warning or supersession missing")
	}
	var count int
	_ = db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=?`, SeasonSearchJobKind).Scan(&count)
	if count != 1 {
		t.Fatalf("%d successors", count)
	}
	if _, err = svc.effectiveImportAllowlist(ctx, dl); err == nil {
		t.Fatal("late original can publish")
	}
}
