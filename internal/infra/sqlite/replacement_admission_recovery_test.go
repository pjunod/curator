package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReplacementRecoveryDirectAdmissionRollsBackChildAndClaim(t *testing.T) {
	db, parent, _ := importControlFixture(t)
	ctx := context.Background()
	const transfer = "direct-replacement-recovery"
	intent := Download{MediaItemID: parent.MediaItemID, Protocol: "torrent", State: "submitting",
		SubmissionPhase: "submitting", ReleaseTitle: "Replacement after interrupted admission", Transfer: transfer}
	assertAbsent := func() {
		t.Helper()
		var children, relations int
		if err := db.R.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM downloads WHERE transfer=?),
			(SELECT count(*) FROM replacement_admissions WHERE parent_id=? AND parent_generation=?)`,
			transfer, parent.ID, parent.GenerationKey).Scan(&children, &relations); err != nil || children != 0 || relations != 0 {
			t.Fatalf("failed admission escaped rollback: downloads=%d relations=%d err=%v", children, relations, err)
		}
	}
	if _, err := db.InsertReplacementDownload(ctx, intent, parent.ID, "stale-generation"); !errors.Is(err, ErrParentDispositionRequested) {
		t.Fatalf("wrong origin generation accepted: %v", err)
	}
	assertAbsent()
	for _, trigger := range []string{
		`CREATE TRIGGER fail_replacement_write BEFORE INSERT ON replacement_admissions
			BEGIN SELECT RAISE(ABORT,'replacement admission storage unavailable'); END`,
		`CREATE TRIGGER fail_replacement_write BEFORE UPDATE OF handoff_log ON downloads
			WHEN NEW.transfer='direct-replacement-recovery' BEGIN SELECT RAISE(ABORT,'replacement admission storage unavailable'); END`,
	} {
		execActivityRecoverySQL(t, db, trigger)
		if _, err := db.InsertReplacementDownload(ctx, intent, parent.ID, parent.GenerationKey); err == nil || !strings.Contains(err.Error(), "replacement admission storage unavailable") {
			t.Fatalf("admission hid its failed write: %v", err)
		}
		assertAbsent()
		execActivityRecoverySQL(t, db, `DROP TRIGGER fail_replacement_write`)
	}
	if _, err := db.InsertReplacementDownload(ctx, intent, parent.ID+100, ""); !errors.Is(err, ErrParentDispositionRequested) {
		t.Fatalf("missing origin accepted by numeric-ID lookup: %v", err)
	}
	child, err := db.InsertReplacementDownload(ctx, intent, parent.ID, "")
	if err != nil || child.ID == 0 || child.GenerationKey == "" || child.SubmissionPhase != "submitting" {
		t.Fatalf("retry did not establish a recoverable exact request: %+v err=%v", child, err)
	}
	var origin string
	if err := db.R.QueryRowContext(ctx, `SELECT parent_generation FROM replacement_admissions WHERE child_id=? AND child_generation=?`,
		child.ID, child.GenerationKey).Scan(&origin); err != nil || origin != parent.GenerationKey {
		t.Fatalf("retried child was bound to %q, want %q, err=%v", origin, parent.GenerationKey, err)
	}
}

func TestReplacementRecoverySeasonClaimRollsBackOnPhaseWriteAndRetiresGoneOrigin(t *testing.T) {
	db, fixture, _ := importControlFixture(t)
	ctx := context.Background()
	parent, err := db.InsertDownloadRecord(ctx, Download{
		MediaItemID: fixture.MediaItemID, Protocol: "torrent", State: "failed", ReleaseTitle: "Replacement origin",
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := db.AcquisitionRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	origin := &SeasonReplacementOrigin{DownloadID: parent.ID, GenerationKey: parent.GenerationKey}
	var intents []Download
	for _, candidate := range []string{"first", "second"} {
		intents = append(intents, Download{MediaItemID: parent.MediaItemID, Season: 1, Protocol: "torrent",
			CandidateKey: candidate, ReleaseTitle: "Season replacement " + candidate, WantableIDs: []string{candidate},
		})
	}
	planID, err := db.AdmitAcquisitionPlan(ctx, AcquisitionPlan{
		MediaItemID: parent.MediaItemID, Season: 1, Decision: []byte("{}"), ReplacementOrigin: origin,
	}, revision, intents)
	if err != nil {
		t.Fatal(err)
	}
	children, err := db.PlanDownloads(ctx, planID)
	if err != nil || len(children) != 2 {
		t.Fatalf("expected two replacement intents: %+v err=%v", children, err)
	}
	first, second := children[0], children[1]
	execActivityRecoverySQL(t, db, `CREATE TRIGGER fail_season_submission BEFORE UPDATE OF submission_phase ON downloads
		WHEN NEW.submission_phase='submitting' BEGIN SELECT RAISE(ABORT,'planned phase unavailable'); END`)
	if claimed, err := db.ClaimPlannedSubmission(ctx, first.ID); claimed || err == nil || !strings.Contains(err.Error(), "planned phase unavailable") {
		t.Fatalf("claim did not report failed phase write: claimed=%v err=%v", claimed, err)
	}
	var relations int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_admissions WHERE child_id=? AND child_generation=?`,
		first.ID, first.GenerationKey).Scan(&relations); err != nil || relations != 0 {
		t.Fatalf("phase failure left a binding: count=%d err=%v", relations, err)
	}
	pending, err := db.GetDownload(ctx, first.ID)
	if err != nil || pending.SubmissionPhase != "pending" {
		t.Fatalf("failed claim did not leave the intent retryable: %+v err=%v", pending, err)
	}
	execActivityRecoverySQL(t, db, `DROP TRIGGER fail_season_submission`)
	if claimed, err := db.ClaimPlannedSubmission(ctx, first.ID); err != nil || !claimed {
		t.Fatalf("healthy origin did not allow retry: claimed=%v err=%v", claimed, err)
	}
	if err := db.RecordDownloadSubmission(ctx, first.ID, first.GenerationKey, first.Transfer, "admitted-handle"); err != nil {
		t.Fatal(err)
	}
	execActivityRecoverySQL(t, db, `DELETE FROM downloads WHERE id=? AND generation_key=?`, parent.ID, parent.GenerationKey)
	execActivityRecoverySQL(t, db, `INSERT INTO downloads(id,media_item_id,release_title,protocol,state,added_at,updated_at)
		VALUES(?,?,'Reused numeric origin','torrent','failed',0,0)`, parent.ID, parent.MediaItemID)
	if claimed, err := db.ClaimPlannedSubmission(ctx, second.ID); err != nil || claimed {
		t.Fatalf("reused numeric ID authorized an old intent: claimed=%v err=%v", claimed, err)
	}
	children, err = db.PlanDownloads(ctx, planID)
	if err != nil || len(children) != 2 {
		t.Fatalf("could not reload season plan children: %+v err=%v", children, err)
	}
	for _, child := range children {
		if child.ID == first.ID && (child.SubmissionPhase != "submitted" || child.Handle != "admitted-handle") {
			t.Fatalf("origin loss changed the acknowledged child: %+v", child)
		}
		if child.ID == second.ID && (child.SubmissionPhase != "rejected" || !child.Superseded || len(child.WantableIDs) != 0) {
			t.Fatalf("origin loss failed to retire the old reservation: %+v", child)
		}
	}
}
