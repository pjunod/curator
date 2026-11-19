package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestActivityDeletionIntentAndWorkerLeaseShareACommit(t *testing.T) {
	db, job, attempt := importControlFixture(t)
	ctx := context.Background()
	identity := ActivityIdentity{ID: job.ID, GenerationKey: job.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	createOperation := func(id string) (ActivityOperation, error) {
		return db.CreateActivityOperation(ctx, id, "delete", identity, []ActivityTarget{{ActivityIdentity: identity}})
	}

	// Fail the final write after the intent, pending replacement cancellation and
	// operation writes. Failure must roll back every earlier write.
	if _, err := db.W.ExecContext(ctx, `INSERT INTO replacement_grabs(download_id,wantable_ids,created_at,next_at) VALUES(?,'["movie:1"]',1,1)`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_activity_enqueue BEFORE INSERT ON jobs
		WHEN NEW.kind='activity.action' BEGIN SELECT RAISE(ABORT,'enqueue failed'); END`); err != nil {
		t.Fatal(err)
	}
	const id = "delete-activity-fixture"
	if _, err := createOperation(id); err == nil || !strings.Contains(err.Error(), "enqueue failed") {
		t.Fatalf("expected final write failure: %v", err)
	}
	if deleting, err := db.DownloadDeletionRequested(ctx, job.ID, job.GenerationKey); err != nil || deleting {
		t.Fatalf("intent escaped the rollback: deleting=%v err=%v", deleting, err)
	}
	if _, err := db.ActivityOperation(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("operation escaped the rollback: %v", err)
	}
	if _, err := db.ClaimJob(ctx, "activity-worker", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("activity job escaped the rollback: %v", err)
	}
	replacement, found, err := db.GetReplacementGrab(ctx, job.ID)
	if err != nil || !found || replacement.Result != "" || replacement.NextAt.IsZero() {
		t.Fatalf("failed delete changed pending replacement: %+v found=%v err=%v", replacement, found, err)
	}

	// On successful retry the worker can claim the operation and immediately
	// observe committed intent and replacement suppression.
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_activity_enqueue`); err != nil {
		t.Fatal(err)
	}
	operation, err := createOperation(id)
	if err != nil || operation.ID != id || operation.State != "running" {
		t.Fatalf("delete operation=%+v err=%v", operation, err)
	}
	leased, err := db.ClaimJob(ctx, "activity-worker", nil, time.Minute)
	if err != nil || leased.Kind != ActivityActionJobKind || leased.State != domain.JobLeased || leased.LeaseOwner != "activity-worker" {
		t.Fatalf("activity lease=%+v err=%v", leased, err)
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(leased.Payload), &payload); err != nil || payload.ID != operation.ID {
		t.Fatalf("lease points to the wrong operation: %q err=%v", leased.Payload, err)
	}
	if deleting, verified, err := db.DownloadDeletionStatus(ctx, job.ID, job.GenerationKey); err != nil || !deleting || verified {
		t.Fatalf("the leased writer did not see unverified deletion intent: deleting=%v verified=%v err=%v", deleting, verified, err)
	}
	if claimed, err := db.ClaimReplacementGrab(ctx, job.ID, time.Now()); err != nil || claimed {
		t.Fatalf("a pending replacement was still claimable: claimed=%v err=%v", claimed, err)
	}
}

func TestActivityDeletionFencesPublicationAndLaterReplacement(t *testing.T) {
	db, job, attempt := importControlFixture(t)
	ctx := context.Background()
	mustSeal(t, db, attempt, "/source/video")
	placement := controlPlacement(job, attempt, "before-delete", "/source/video", "/library/video")
	if err := db.PreparePlacement(ctx, placement); err != nil {
		t.Fatal(err)
	}
	identity := ActivityIdentity{ID: job.ID, GenerationKey: job.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	if _, err := db.CreateActivityOperation(ctx, "delete-publication-fixture", "delete", identity, []ActivityTarget{{ActivityIdentity: identity}}); err != nil {
		t.Fatal(err)
	}

	// The import worker prepared this write before Delete. It has to hit the
	// durable fence even before the deletion worker can signal cancellation.
	if err := db.AuthorizeImportPlacement(ctx, placement); err == nil {
		t.Fatal("a prepared import obtained publication authority after deletion intent")
	}
	current, err := db.GetImportAttempt(ctx, job.ID, job.GenerationKey)
	if err != nil || current.PendingPublications != 0 {
		t.Fatalf("authorization escaped its failed transaction: %+v err=%v", current, err)
	}

	// Exercise the insertion used by a late terminal or cleanup fact. The
	// database must suppress a new grab for this deleted acquisition.
	if _, err := db.W.ExecContext(ctx, `INSERT INTO replacement_grabs(download_id,wantable_ids,created_at,next_at) VALUES(?,'["movie:1"]',1,1)`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := db.GetReplacementGrab(ctx, job.ID); err != nil || found {
		t.Fatalf("late observation scheduled a replacement: found=%v err=%v", found, err)
	}
}

func TestActivityLatePublicationReopensOnlyItsDownloadGeneration(t *testing.T) {
	db, job, attempt := importControlFixture(t)
	ctx := context.Background()
	current := ActivityIdentity{ID: job.ID, GenerationKey: job.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	previous := ActivityIdentity{ID: job.ID, GenerationKey: "earlier-generation", Epoch: "0"}

	operation, err := db.CreateActivityOperation(ctx, "current-delete", "delete", current, []ActivityTarget{{ActivityIdentity: current}})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := db.DispositionRevision(ctx, job.ID, job.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyDownloadDeletion(ctx, job.ID, job.GenerationKey, revision); err != nil {
		t.Fatal(err)
	}
	operation.Targets[0].State = "succeeded"
	if err := db.SaveActivityTarget(ctx, &operation, 0); err != nil {
		t.Fatal(err)
	}
	old, err := db.CreateActivityOperation(ctx, "stale-delete", "delete", previous, []ActivityTarget{{ActivityIdentity: previous}})
	if err != nil || old.Blocked != 1 {
		t.Fatalf("stale deletion operation=%+v err=%v", old, err)
	}

	// Model a preserved receipt from earlier use of this numeric job ID. Put a
	// newer rejected command against that generation at the head of the log.
	if _, err := db.W.ExecContext(ctx, `INSERT INTO download_dispositions(download_id,generation_key,requested_at,verified_at,state)
		VALUES(?,?,1,2,'verified')`, previous.ID, previous.GenerationKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE activity_operations SET created_at=(SELECT created_at+1 FROM activity_operations WHERE id=?) WHERE id=?`, operation.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	for {
		leased, err := db.ClaimJob(ctx, "activity-worker", nil, time.Minute)
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil || leased.Kind != ActivityActionJobKind {
			t.Fatalf("claiming fixture worker: %+v err=%v", leased, err)
		}
		if err := db.CompleteJob(ctx, leased.ID); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.InsertPublication(ctx, PublicationWrite{
		DownloadID: job.ID, Generation: "late-source-generation", Kind: "salvage", Path: "/completed/later",
		Source: "none", Cohort: "live",
	}); err != nil {
		t.Fatal(err)
	}
	// A worker holding the previous operation snapshot must not overwrite the
	// reopened target if it resumes after the late publication transaction.
	if err := db.SaveActivityTarget(ctx, &operation, 0); err == nil {
		t.Fatal("a stale worker overwrote the reopened deletion")
	}
	reopened, err := db.ActivityOperation(ctx, operation.ID)
	if err != nil || reopened.State != "running" || len(reopened.Targets) != 1 || reopened.Targets[0].State != "pending" {
		t.Fatalf("late publication did not reopen current delete: %+v err=%v", reopened, err)
	}
	stale, err := db.ActivityOperation(ctx, old.ID)
	if err != nil || stale.State != "completed" || stale.Blocked != 1 {
		t.Fatalf("late publication reopened an unrelated generation: %+v err=%v", stale, err)
	}
	if deleting, verified, err := db.DownloadDeletionStatus(ctx, previous.ID, previous.GenerationKey); err != nil || !deleting || !verified {
		t.Fatalf("late publication reset a previous generation's receipt: deleting=%v verified=%v err=%v", deleting, verified, err)
	}
	if deleting, verified, err := db.DownloadDeletionStatus(ctx, current.ID, current.GenerationKey); err != nil || !deleting || verified {
		t.Fatalf("late publication failed to reset its own receipt: deleting=%v verified=%v err=%v", deleting, verified, err)
	}
	leased, err := db.ClaimJob(ctx, "activity-worker", nil, time.Minute)
	if err != nil || leased.Kind != ActivityActionJobKind || leased.DedupeKey != "activity:"+operation.ID {
		t.Fatalf("late publication queued the wrong operation: %+v err=%v", leased, err)
	}
	if _, err := db.ClaimJob(ctx, "activity-worker-2", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late publication queued an extra operation: %v", err)
	}
}

func TestActivityBulkProgressCannotOverwriteNewDispositionRevision(t *testing.T) {
	db, job, attempt := importControlFixture(t)
	ctx := context.Background()
	other, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: job.MediaItemID, Protocol: "manual", State: "failed", ReleaseTitle: "second bulk member"})
	if err != nil {
		t.Fatal(err)
	}
	first := ActivityIdentity{ID: job.ID, GenerationKey: job.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	second := ActivityIdentity{ID: other.ID, GenerationKey: other.GenerationKey, Epoch: "0"}
	bulk, err := db.CreateActivityOperation(ctx, "old-bulk", "delete", []ActivityIdentity{first, second},
		[]ActivityTarget{{ActivityIdentity: first}, {ActivityIdentity: second}})
	if err != nil {
		t.Fatal(err)
	}
	oldRevision, err := db.DispositionRevision(ctx, first.ID, first.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyDownloadDeletion(ctx, first.ID, first.GenerationKey, oldRevision); err != nil {
		t.Fatal(err)
	}
	bulk.Targets[0].State = "succeeded"
	bulk.Targets[0].DispositionRevision = oldRevision
	if err := db.SaveActivityTarget(ctx, &bulk, 0); err != nil {
		t.Fatal(err)
	}

	latest, err := db.CreateActivityOperation(ctx, "new-single", "delete", first, []ActivityTarget{{ActivityIdentity: first}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE activity_operations SET created_at=(SELECT created_at+1 FROM activity_operations WHERE id=?) WHERE id=?`, bulk.ID, latest.ID); err != nil {
		t.Fatal(err)
	}
	late, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: first.ID, Kind: "salvage", Generation: "late-revision", Path: "/completed/late-revision", Source: "none", Cohort: "live"})
	if err != nil {
		t.Fatal(err)
	}
	newRevision, err := db.DispositionRevision(ctx, first.ID, first.GenerationKey)
	if err != nil || newRevision <= oldRevision {
		t.Fatalf("late publication did not advance the disposition revision: old=%d current=%d err=%v", oldRevision, newRevision, err)
	}
	bulk.Targets[1].State = "running"
	if err := db.SaveActivityTarget(ctx, &bulk, 1); err != nil {
		t.Fatal(err)
	}
	assertState := func(want string) {
		t.Helper()
		var state string
		err := db.R.QueryRowContext(ctx, `SELECT state FROM download_dispositions WHERE download_id=? AND generation_key=?`, first.ID, first.GenerationKey).Scan(&state)
		if err != nil || state != want {
			t.Fatalf("old bulk member changed the canonical disposition: state=%q want=%q err=%v", state, want, err)
		}
	}
	assertState("requested")
	if err := db.SetPublicationState(ctx, late, "removed", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyDownloadDeletion(ctx, first.ID, first.GenerationKey, oldRevision); err == nil {
		t.Fatal("an earlier worker verified the newer disposition")
	}
	if err := db.VerifyDownloadDeletion(ctx, first.ID, first.GenerationKey, newRevision); err != nil {
		t.Fatal(err)
	}
	staleTarget := bulk.Targets[0]
	staleTarget.State = "blocked"
	if err := db.SetDispositionProgress(ctx, staleTarget); err != nil {
		t.Fatal(err)
	}
	bulk.Targets[1].State = "blocked"
	if err := db.SaveActivityTarget(ctx, &bulk, 1); err != nil {
		t.Fatal(err)
	}
	assertState("verified")
}

func TestActivityLateReplacementPublicationReopensParentDeletion(t *testing.T) {
	t.Run("parent operation alone", func(t *testing.T) {
		checkLateReplacementPublicationReplay(t, false)
	})
	t.Run("parent and later direct child operations", func(t *testing.T) {
		checkLateReplacementPublicationReplay(t, true)
	})
}

func checkLateReplacementPublicationReplay(t *testing.T, directChildDelete bool) {
	t.Helper()
	db, parent, attempt := importControlFixture(t)
	ctx := context.Background()
	child, err := db.InsertReplacementDownload(ctx, Download{
		MediaItemID: parent.MediaItemID, Protocol: "torrent", State: "submitting",
		ReleaseTitle: "Replacement awaiting admission", SubmissionPhase: "submitting",
		Transfer: "pending-replacement-transfer", ExecutionPayload: "{}",
	}, parent.ID, parent.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}

	type completedDelete struct {
		identity ActivityIdentity
		jobID    int64
	}
	expected := map[string]completedDelete{}
	finishDelete := func(id string, identity ActivityIdentity, dispositions ...Download) {
		t.Helper()
		operation, err := db.CreateActivityOperation(ctx, id, "delete", identity, []ActivityTarget{{ActivityIdentity: identity}})
		if err != nil {
			t.Fatal(err)
		}
		dedupe := "activity:" + id
		original, err := db.ClaimJob(ctx, "activity-worker", nil, time.Minute)
		if err != nil || original.Kind != ActivityActionJobKind || original.DedupeKey != dedupe {
			t.Fatalf("claim initial delete: %+v err=%v", original, err)
		}
		for _, dl := range dispositions {
			revision, err := db.DispositionRevision(ctx, dl.ID, dl.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.VerifyDownloadDeletion(ctx, dl.ID, dl.GenerationKey, revision); err != nil {
				t.Fatal(err)
			}
		}
		operation.Targets[0].State = "succeeded"
		if err := db.SaveActivityTarget(ctx, &operation, 0); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteJob(ctx, original.ID); err != nil {
			t.Fatal(err)
		}
		completed, err := db.ActivityOperation(ctx, id)
		if err != nil || completed.State != "completed" || completed.Succeeded != 1 {
			t.Fatalf("fixture did not complete deletion %s: %+v err=%v", id, completed, err)
		}
		expected[dedupe] = completedDelete{identity: identity, jobID: original.ID}
	}

	parentIdentity := ActivityIdentity{ID: parent.ID, GenerationKey: parent.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	finishDelete("delete-with-replacement", parentIdentity, child, parent)
	if directChildDelete {
		childIdentity := ActivityIdentity{ID: child.ID, GenerationKey: child.GenerationKey, Epoch: "0"}
		finishDelete("later-direct-child-delete", childIdentity, child)
	}

	if _, err := db.InsertPublication(ctx, PublicationWrite{
		DownloadID: child.ID, Generation: "late-replacement-artifact", Kind: "salvage",
		Path: "/completed/late-replacement", Source: "none", Cohort: "live",
	}); err != nil {
		t.Fatal(err)
	}
	for _, dl := range []Download{child, parent} {
		requested, verified, err := db.DownloadDeletionStatus(ctx, dl.ID, dl.GenerationKey)
		if err != nil || !requested || verified {
			t.Fatalf("late child publication did not reopen disposition %d: requested=%v verified=%v err=%v", dl.ID, requested, verified, err)
		}
	}
	for dedupe, want := range expected {
		reopened, err := db.ActivityOperation(ctx, strings.TrimPrefix(dedupe, "activity:"))
		if err != nil || reopened.State != "running" || len(reopened.Targets) != 1 ||
			reopened.Targets[0].ActivityIdentity != want.identity || reopened.Targets[0].State != "pending" {
			t.Fatalf("late child publication did not reopen the original member of %s: %+v err=%v", dedupe, reopened, err)
		}
	}
	for len(expected) > 0 {
		replay, err := db.ClaimJob(ctx, "activity-worker", nil, time.Minute)
		want, found := expected[replay.DedupeKey]
		if err != nil || !found || replay.ID == want.jobID || replay.Kind != ActivityActionJobKind {
			t.Fatalf("late child publication did not queue one replay per operation: %+v err=%v", replay, err)
		}
		delete(expected, replay.DedupeKey)
	}
	if _, err := db.ClaimJob(ctx, "another-worker", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late child publication queued an extra operation: %v", err)
	}
}
