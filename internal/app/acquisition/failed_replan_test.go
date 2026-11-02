package acquisition

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Field report 2026-11-02: the season planner re-chose the identical NZB
// (same indexer, same GUID) seconds after it failed and was blocklisted by
// identity; dispatch refused it and the season stalled.
func TestPlannerCandidatesHonourTheReleaseIdentityBlocklist(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	failed := ports.Release{Title: "Test.Show.S01E01.2160p.WEB.h265-GRP", Indexer: "idx", IndexerID: 1, GUID: "https://idx.example/details/abc"}
	if svc.skipCandidateFor(ctx, itemID, failed) {
		t.Fatal("a release nobody blocklisted was skipped")
	}
	if err := db.AddReleaseIdentityBlocklist(ctx, itemID, failed.IndexerID, failed.GUID, "FAILURE/HEALTH"); err != nil {
		t.Fatal(err)
	}
	if !svc.skipCandidateFor(ctx, itemID, failed) {
		t.Fatal("identity-blocklisted release is still a planner candidate")
	}
	other := failed
	other.GUID = "https://idx.example/details/another-post"
	if svc.skipCandidateFor(ctx, itemID, other) {
		t.Fatal("a different post of the same title must stay eligible")
	}
}

// Field report 2026-11-02: ten S01 episodes failed and their replacement
// comparison merged into a season job parked until the next discovery epoch,
// almost a day away. A failed grab must make it due now, and a comparison
// that is already running must rerun when it finishes.
func TestFailedGrabMakesAParkedSeasonComparisonDueAndRerunsARunningOne(t *testing.T) {
	svc, db, itemID, _ := seriesFixture(t)
	ctx := context.Background()
	if err := svc.enqueueSeason(ctx, itemID, 0, 1, "backlog", true); err != nil {
		t.Fatal(err)
	}
	parkedUntil := time.Now().Add(20 * time.Hour).UnixMilli()
	if _, err := db.W.ExecContext(ctx, `UPDATE jobs SET run_after=? WHERE kind=?`, parkedUntil, SeasonSearchJobKind); err != nil {
		t.Fatal(err)
	}
	if err := svc.enqueueSeason(ctx, itemID, 0, 1, seasonTriggerFailedAcquisition, true); err != nil {
		t.Fatal(err)
	}
	var runAfter, generation int64
	if err := db.R.QueryRowContext(ctx, `SELECT run_after, COALESCE(json_extract(payload,'$.replanGeneration'),0) FROM jobs WHERE kind=? AND state='queued'`, SeasonSearchJobKind).Scan(&runAfter, &generation); err != nil {
		t.Fatal(err)
	}
	if runAfter > time.Now().UnixMilli() || generation != 1 {
		t.Fatalf("failed grab left the comparison parked: run_after in %s, replanGeneration=%d", time.Until(time.UnixMilli(runAfter)), generation)
	}

	job, err := db.ClaimJob(ctx, "running-comparison", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var started seasonCheckpoint
	if err := json.Unmarshal([]byte(job.Payload), &started); err != nil {
		t.Fatal(err)
	}
	if svc.seasonReplanRequested(ctx, job.ID, started) {
		t.Fatal("a comparison must not rerun for a request it already claimed")
	}
	if err := svc.enqueueSeason(ctx, itemID, 0, 1, seasonTriggerFailedAcquisition, true); err != nil {
		t.Fatal(err)
	}
	// The running comparison saves its own checkpoint afterwards; that write
	// must not erase the request that arrived while it ran.
	if err := svc.saveSeasonCheckpoint(ctx, job, started); err != nil {
		t.Fatal(err)
	}
	if !svc.seasonReplanRequested(ctx, job.ID, started) {
		t.Fatal("a failed grab during a running comparison was lost")
	}
	// A deferral written after the request must not park it for a day.
	if err := db.DeferClaimedJob(ctx, job, "new discovery epoch", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT run_after FROM jobs WHERE id=?`, job.ID).Scan(&runAfter); err != nil {
		t.Fatal(err)
	}
	if runAfter > time.Now().UnixMilli() {
		t.Fatalf("deferral parked a requested replan for %s", time.Until(time.UnixMilli(runAfter)))
	}
	// With no comparison holding the key, the request queues itself.
	if _, err := db.W.ExecContext(ctx, `UPDATE jobs SET state='done' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.enqueueSeason(ctx, itemID, 0, 1, seasonTriggerFailedAcquisition, true); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=? AND state='queued'`, SeasonSearchJobKind).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("failed grab after the comparison finished queued %d comparisons (err=%v)", queued, err)
	}
}

// Review finding: a rerun requested by a failed grab must not carry the
// finished run's Started evidence, or it parks behind its own plan as an open
// comparison and holds the admission slot every other season waits on.
func TestReplanRerunStartsFromAFreshCheckpoint(t *testing.T) {
	svc, db, itemID, _ := seriesFixture(t)
	ctx := context.Background()
	if err := svc.enqueueSeason(ctx, itemID, 0, 1, "backlog", true); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimJob(ctx, "comparison", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.enqueueSeason(ctx, itemID, 0, 1, seasonTriggerFailedAcquisition, true); err != nil {
		t.Fatal(err)
	}
	err = svc.handleSeasonSearch(ctx, job)
	var deferred interface{ Error() string }
	if err == nil {
		t.Fatal("a requested replan must rerun, not complete")
	}
	deferred = err
	saved, e := db.GetJob(ctx, job.ID)
	if e != nil {
		t.Fatal(e)
	}
	var p seasonCheckpoint
	if e := json.Unmarshal([]byte(saved.Payload), &p); e != nil {
		t.Fatal(e)
	}
	if !p.Started.IsZero() || len(p.Scopes) != 0 || len(p.Releases) != 0 || p.ReplanGeneration != 1 {
		t.Fatalf("rerun kept the consumed comparison (err=%v): started=%v scopes=%d releases=%d replan=%d", deferred, p.Started, len(p.Scopes), len(p.Releases), p.ReplanGeneration)
	}
}
