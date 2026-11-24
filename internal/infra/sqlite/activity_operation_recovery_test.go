package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func execActivityRecoverySQL(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.W.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestActivityRecoveryFailedMembershipWritesRollBackDescendantIntents(t *testing.T) {
	for _, phase := range []struct {
		name  string
		table string
		when  string
	}{
		{"operation", "activity_operations", "1"},
		{"second member", "activity_operation_targets", "EXISTS(SELECT 1 FROM activity_operation_targets WHERE operation_id=NEW.operation_id)"},
		{"descendant link", "activity_disposition_links", "NEW.download_id<>NEW.target_id"},
	} {
		t.Run(phase.name, func(t *testing.T) {
			db, parent, attempt := importControlFixture(t)
			ctx := context.Background()
			child, err := db.InsertReplacementDownload(ctx, Download{
				MediaItemID: parent.MediaItemID, Protocol: "torrent", State: "submitting",
				SubmissionPhase: "submitting", ReleaseTitle: "Unacknowledged replacement", Transfer: "recovery-child",
			}, parent.ID, parent.GenerationKey)
			if err != nil {
				t.Fatal(err)
			}
			other, err := db.InsertDownloadRecord(ctx, Download{
				MediaItemID: parent.MediaItemID, Protocol: "manual", State: "failed", ReleaseTitle: "Other requested deletion",
			})
			if err != nil {
				t.Fatal(err)
			}
			parentIdentity := ActivityIdentity{ID: parent.ID, GenerationKey: parent.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
			otherIdentity := ActivityIdentity{ID: other.ID, GenerationKey: other.GenerationKey, Epoch: "0"}
			payload := []ActivityIdentity{parentIdentity, otherIdentity}
			create := func() (ActivityOperation, error) {
				return db.CreateActivityOperation(ctx, "delete-recovery", "delete", payload,
					[]ActivityTarget{{ActivityIdentity: parentIdentity}, {ActivityIdentity: otherIdentity}})
			}
			execActivityRecoverySQL(t, db, `CREATE TRIGGER fail_activity_membership BEFORE INSERT ON `+phase.table+
				` WHEN `+phase.when+` BEGIN SELECT RAISE(ABORT,'activity membership unavailable'); END`)
			if _, err := create(); err == nil || !strings.Contains(err.Error(), "activity membership unavailable") {
				t.Fatalf("operation did not report the failed write: %v", err)
			}
			for _, dl := range []Download{parent, child, other} {
				if requested, err := db.DownloadDeletionRequested(ctx, dl.ID, dl.GenerationKey); err != nil || requested {
					t.Fatalf("download %d escaped rollback: requested=%v err=%v", dl.ID, requested, err)
				}
			}
			if _, found, err := db.ReplayActivityOperation(ctx, "delete-recovery", payload); err != nil || found {
				t.Fatalf("failed command can be replayed: found=%v err=%v", found, err)
			}
			if _, err := db.ClaimJob(ctx, "activity-recovery", nil, time.Minute); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed operation left a runnable job: %v", err)
			}
			var relation string
			if err := db.R.QueryRowContext(ctx, `SELECT phase FROM replacement_admissions WHERE child_id=? AND child_generation=?`, child.ID, child.GenerationKey).Scan(&relation); err != nil || relation != "admitting" {
				t.Fatalf("failed operation cancelled its unacknowledged child: phase=%q err=%v", relation, err)
			}

			execActivityRecoverySQL(t, db, `DROP TRIGGER fail_activity_membership`)
			retried, err := create()
			if err != nil || retried.Total != 2 || retried.Completed != 0 {
				t.Fatalf("same request ID could not retry: %+v err=%v", retried, err)
			}
			if requested, err := db.DownloadDeletionRequested(ctx, child.ID, child.GenerationKey); err != nil || !requested {
				t.Fatalf("retry did not enroll the unacknowledged child: requested=%v err=%v", requested, err)
			}
		})
	}
}

func TestActivityRecoveryTargetAndEventCommitTogether(t *testing.T) {
	db, dl, attempt := importControlFixture(t)
	ctx := context.Background()
	identity := ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	operation, err := db.CreateActivityOperation(ctx, "retry-event", "retry", identity, []ActivityTarget{{ActivityIdentity: identity}})
	if err != nil {
		t.Fatal(err)
	}
	priorVersion := operation.Version
	operation.Targets[0].State = "succeeded"
	operation.Targets[0].Message = "Source imported"
	for _, fault := range []struct {
		action string
		table  string
	}{
		{"UPDATE", "activity_operation_targets"},
		{"INSERT", "activity_action_events"},
	} {
		execActivityRecoverySQL(t, db, `CREATE TRIGGER fail_activity_transition BEFORE `+fault.action+
			` ON `+fault.table+` BEGIN SELECT RAISE(ABORT,'activity transition unavailable'); END`)
		if err := db.SaveActivityTarget(ctx, &operation, 0); err == nil || !strings.Contains(err.Error(), "activity transition unavailable") {
			t.Fatalf("failed %s was reported successful: %v", fault.table, err)
		}
		current, err := db.ActivityOperation(ctx, operation.ID)
		if err != nil || current.Version != priorVersion || operation.Version != priorVersion || current.Targets[0].State != "pending" {
			t.Fatalf("failed %s committed some of the transition: %+v err=%v", fault.table, current, err)
		}
		execActivityRecoverySQL(t, db, `DROP TRIGGER fail_activity_transition`)
	}
	stale, err := db.ActivityOperation(ctx, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveActivityTarget(ctx, &operation, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveActivityTarget(ctx, &stale, 0); err == nil {
		t.Fatal("stale worker reverted a committed transition")
	}
	current, err := db.ActivityOperation(ctx, operation.ID)
	if err != nil || current.Version == priorVersion || current.Succeeded != 1 || current.Targets[0].ActivityIdentity != identity {
		t.Fatalf("retried transition did not finish its exact member: %+v err=%v", current, err)
	}
	if err := db.SaveActivityOperation(ctx, &current); err != nil {
		t.Fatal(err)
	}
	var events int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM activity_action_events WHERE operation_id=? AND download_id=? AND generation_key=? AND step=? AND message=?`,
		operation.ID, dl.ID, dl.GenerationKey, operation.Targets[0].State, operation.Targets[0].Message).Scan(&events); err != nil || events != 1 {
		t.Fatalf("successful transition did not leave exactly one event: count=%d err=%v", events, err)
	}
}

func TestActivityRecoveryRequestReplayKeepsFrozenMembership(t *testing.T) {
	db, dl, attempt := importControlFixture(t)
	ctx := context.Background()
	identity := ActivityIdentity{ID: dl.ID, GenerationKey: dl.GenerationKey, Epoch: strconv.FormatInt(attempt.Epoch, 10)}
	const selection = "expired-selection"
	payload := map[string]string{"selection": selection, "action": "delete"}
	target := ActivityTarget{ActivityIdentity: identity}
	if err := db.SaveActivitySelection(ctx, selection, []ActivityIdentity{identity}); err != nil {
		t.Fatal(err)
	}
	const request = "frozen-delete"
	if _, err := db.CreateActivityOperation(ctx, request, "delete", payload, []ActivityTarget{target}); err != nil {
		t.Fatal(err)
	}
	execActivityRecoverySQL(t, db, `UPDATE activity_selections SET created_at=0 WHERE id=?`, selection)
	if _, err := db.ActivitySelection(ctx, selection); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fixture selection did not expire: %v", err)
	}
	recovered, found, err := db.ReplayActivityOperation(ctx, request, payload)
	if err != nil || !found || recovered.Total != 1 || recovered.Targets[0].ActivityIdentity != identity {
		t.Fatalf("accepted request could not be replayed after selection expiry: %+v found=%v err=%v", recovered, found, err)
	}
	target.Epoch = strconv.FormatInt(attempt.Epoch+1, 10)
	same, err := db.CreateActivityOperation(ctx, request, "delete", payload, []ActivityTarget{target})
	if err != nil || same.Total != 1 || same.Targets[0].ActivityIdentity != identity {
		t.Fatalf("retry substituted a newer identity: %+v err=%v", same, err)
	}
	different := map[string]string{"selection": selection, "action": "retry"}
	if _, found, err := db.ReplayActivityOperation(ctx, request, different); !found || err == nil {
		t.Fatalf("changed action replay reused a request ID: found=%v err=%v", found, err)
	}
	if _, err := db.CreateActivityOperation(ctx, request, "retry", different, []ActivityTarget{target}); err == nil {
		t.Fatal("changed action created an operation under a prior request ID")
	}
}
