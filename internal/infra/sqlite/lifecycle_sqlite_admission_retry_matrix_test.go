package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func insertLifecycleAdmission(t *testing.T, db *DB, title, transfer, phase, intent string, exact bool) Download {
	t.Helper()
	ctx := context.Background()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: title, Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{
		MediaItemID: itemID, ReleaseTitle: title, Protocol: "nzb", ClientID: 71,
		State: "grabbed", Transfer: transfer, RequestTokenIdempotent: true,
		SubmissionPhase: phase, Intent: intent, ExactTransferCorrelation: exact,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dl
}

func TestRunnerSubmissionReceiptRejectsStaleAndConflictingAcknowledgements(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	dl := insertLifecycleAdmission(t, db, "Submission identity matrix", "transfer-submission-matrix", "uncertain", "automatic", false)

	for name, args := range map[string]struct {
		id                           int64
		generation, transfer, handle string
	}{
		"missing generation": {dl.ID, "", dl.Transfer, "job-1"},
		"wrong transfer":     {dl.ID, dl.GenerationKey, "reused-transfer", "job-1"},
		"missing handle":     {dl.ID, dl.GenerationKey, dl.Transfer, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.RecordDownloadSubmission(ctx, args.id, args.generation, args.transfer, args.handle); err == nil {
				t.Fatalf("invalid submission receipt was accepted: %+v", args)
			}
			current, err := db.GetDownload(ctx, dl.ID)
			if err != nil || current.SubmissionPhase != "uncertain" || current.Handle != "" {
				t.Fatalf("invalid receipt changed the pending request: %+v err=%v", current, err)
			}
		})
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "job-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "job-1"); err != nil {
		t.Fatalf("exact lost-ack replay was not idempotent: %v", err)
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "job-reused"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("different native job handle error=%v, want identity conflict", err)
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, "stale-generation", dl.Transfer, "job-1"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("stale generation error=%v, want identity conflict", err)
	}
	current, err := db.GetDownload(ctx, dl.ID)
	if err != nil || current.SubmissionPhase != "submitted" || current.Handle != "job-1" {
		t.Fatalf("conflicting replay altered the committed receipt: %+v err=%v", current, err)
	}
}

func TestRunnerSubmissionDefinitiveRefusalIsTerminalAndIdentityScoped(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	dl := insertLifecycleAdmission(t, db, "Submission refusal matrix", "transfer-refusal-matrix", "pending", "automatic", false)
	for name, args := range map[string]struct {
		id                   int64
		generation, transfer string
	}{
		"zero id":          {0, dl.GenerationKey, dl.Transfer},
		"stale generation": {dl.ID, "stale-generation", dl.Transfer},
		"wrong token":      {dl.ID, dl.GenerationKey, "other-token"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.RejectDownloadSubmission(ctx, args.id, args.generation, args.transfer, "refused"); err == nil {
				t.Fatalf("invalid refusal identity accepted: %+v", args)
			}
		})
	}
	if err := db.RejectDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "permanent client refusal"); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetDownload(ctx, dl.ID)
	if err != nil || current.State != "failed" || current.SubmissionPhase != "rejected" || current.Error != "permanent client refusal" {
		t.Fatalf("refusal did not settle atomically: %+v err=%v", current, err)
	}
	if err := db.RejectDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "stale refusal"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("terminal refusal replay error=%v", err)
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "job-after-refusal"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("definitively rejected request accepted a later handle: %v", err)
	}
}

func TestHandlelessRunnerAdmissionRequiresExactCorrelationAndPersistsManualWait(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	manual := insertLifecycleAdmission(t, db, "Manual handleless", "manual-handleless-token", "uncertain", "manual_additional", true)
	automatic := insertLifecycleAdmission(t, db, "Automatic handleless", "automatic-handleless-token", "submitting", "automatic", true)
	noCorrelation := insertLifecycleAdmission(t, db, "No correlation", "uncorrelated-handleless-token", "uncertain", "automatic", false)
	for name, args := range map[string]struct {
		id                   int64
		generation, transfer string
	}{
		"missing transfer":          {manual.ID, manual.GenerationKey, ""},
		"wrong generation":          {manual.ID, "stale-generation", manual.Transfer},
		"no exact-token capability": {noCorrelation.ID, noCorrelation.GenerationKey, noCorrelation.Transfer},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.RecordHandlelessSubmission(ctx, args.id, args.generation, args.transfer); err == nil {
				t.Fatalf("invalid handleless receipt accepted: %+v", args)
			}
		})
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET handle='native-should-not-be-cleared' WHERE id=?`, automatic.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordHandlelessSubmission(ctx, automatic.ID, automatic.GenerationKey, automatic.Transfer); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("handleless receipt overwrote an existing native handle: %v", err)
	}
	manualRetryNow := time.Unix(1_900_000_000, 0)
	manualProviderDeadline := manualRetryNow.Add(20 * time.Minute)
	if err := db.ScheduleManualAdmissionRetry(ctx, manual.ID, manualRetryNow, "manual Add acknowledgement uncertain", manualProviderDeadline); err != nil {
		t.Fatal(err)
	}
	var expectedManualRetryAt, expectedManualBlockedAt int64
	var manualRetryAt, manualBlockedAt int64
	var expectedManualAttempts int
	var manualAttempts int
	if err := db.R.QueryRowContext(ctx, `SELECT manual_retry_at,manual_retry_attempts,manual_blocked_at FROM downloads WHERE id=?`, manual.ID).Scan(&expectedManualRetryAt, &expectedManualAttempts, &expectedManualBlockedAt); err != nil {
		t.Fatal(err)
	}
	if expectedManualRetryAt != manualProviderDeadline.UnixMilli() || expectedManualAttempts != 1 || expectedManualBlockedAt != manualRetryNow.UnixMilli() {
		t.Fatalf("manual provider retry schedule not established: at=%d attempts=%d blocked=%d", expectedManualRetryAt, expectedManualAttempts, expectedManualBlockedAt)
	}
	if _, err := db.W.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER reject_handleless_receipt BEFORE UPDATE OF submission_phase ON downloads WHEN OLD.id=%d AND NEW.submission_phase='submitted' BEGIN SELECT RAISE(ABORT,'injected handleless receipt failure'); END`, manual.ID)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordHandlelessSubmission(ctx, manual.ID, manual.GenerationKey, manual.Transfer); err == nil {
		t.Fatal("handleless acknowledgement passed an injected persistence failure")
	}
	if err := db.W.QueryRowContext(ctx, `SELECT manual_retry_at,manual_retry_attempts,manual_blocked_at FROM downloads WHERE id=?`, manual.ID).Scan(&manualRetryAt, &manualAttempts, &manualBlockedAt); err != nil {
		t.Fatal(err)
	}
	if manualRetryAt != expectedManualRetryAt || manualAttempts != expectedManualAttempts || manualBlockedAt != expectedManualBlockedAt {
		t.Fatalf("failed acknowledgement changed retry schedule: at=%d attempts=%d blocked=%d", manualRetryAt, manualAttempts, manualBlockedAt)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_handleless_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordHandlelessSubmission(ctx, manual.ID, manual.GenerationKey, manual.Transfer); err != nil {
		t.Fatal(err)
	}
	settled, err := db.GetDownload(ctx, manual.ID)
	if scanErr := db.R.QueryRowContext(ctx, `SELECT manual_retry_at,manual_retry_attempts,manual_blocked_at FROM downloads WHERE id=?`, manual.ID).Scan(&manualRetryAt, &manualAttempts, &manualBlockedAt); scanErr != nil {
		t.Fatal(scanErr)
	}
	if err != nil || settled.SubmissionPhase != "submitted" || settled.Handle != "" || manualAttempts != expectedManualAttempts || manualRetryAt != expectedManualRetryAt || manualBlockedAt != expectedManualBlockedAt {
		t.Fatalf("manual handleless admission lost its token-correlation wait: %+v err=%v", settled, err)
	}
	if err := db.RecordHandlelessSubmission(ctx, manual.ID, manual.GenerationKey, manual.Transfer); err != nil {
		t.Fatalf("exact handleless receipt replay was not idempotent: %v", err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT manual_retry_at,manual_retry_attempts,manual_blocked_at FROM downloads WHERE id=?`, manual.ID).Scan(&manualRetryAt, &manualAttempts, &manualBlockedAt); err != nil || manualRetryAt != expectedManualRetryAt || manualAttempts != expectedManualAttempts || manualBlockedAt != expectedManualBlockedAt {
		t.Fatalf("exact receipt replay rewrote the manual retry schedule: at=%d attempts=%d blocked=%d err=%v", manualRetryAt, manualAttempts, manualBlockedAt, err)
	}
	if err := db.RecordHandlelessSubmission(ctx, automatic.ID, automatic.GenerationKey, automatic.Transfer); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("native handle was cleared on automatic request: %v", err)
	}
}

func TestRunnerSubmissionRetryDeadlineAndWarningRemainBoundToLiveGeneration(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	dl := insertLifecycleAdmission(t, db, "Submission retry schedule", "retry-schedule-token", "uncertain", "automatic", false)
	now := time.Unix(1_900_000_000, 0)
	provider := now.Add(9 * time.Minute)
	for index, interval := range []time.Duration{9 * time.Minute, time.Minute, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		retry, err := db.ScheduleRunnerSubmissionRetry(ctx, dl.ID, dl.GenerationKey, now, fmt.Sprintf("temporary failure %d", index+1), func() []time.Time {
			if index == 0 {
				return []time.Time{provider}
			}
			return nil
		}()...)
		if err != nil || retry.Attempts != index+1 || retry.BlockedAt.IsZero() {
			t.Fatalf("attempt %d retry=%+v err=%v", index+1, retry, err)
		}
		wantDeadline := now.Add(interval)
		if index == 0 {
			wantDeadline = provider
		}
		var due int64
		if err := db.R.QueryRowContext(ctx, `SELECT runner_submission_retry_at FROM downloads WHERE id=?`, dl.ID).Scan(&due); err != nil || !time.UnixMilli(due).Equal(wantDeadline) {
			t.Fatalf("attempt %d deadline=%v want=%v err=%v", index+1, time.UnixMilli(due), wantDeadline, err)
		}
		now = wantDeadline
	}
	if err := db.UpdateRunnerSubmissionWarning(ctx, dl.ID, dl.GenerationKey, "manual review after prolonged wait"); err != nil {
		t.Fatal(err)
	}
	if due, err := db.RunnerSubmissionRetryDue(ctx, dl.ID, dl.GenerationKey, now.Add(-time.Millisecond)); err != nil || due {
		t.Fatalf("retry ignored its future deadline: due=%v err=%v", due, err)
	}
	if due, err := db.RunnerSubmissionRetryDue(ctx, dl.ID, dl.GenerationKey, now); err != nil || !due {
		t.Fatalf("retry was not due at its exact persisted deadline: due=%v err=%v", due, err)
	}
	if err := db.RejectDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "permanent refusal"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateRunnerSubmissionWarning(ctx, dl.ID, dl.GenerationKey, "stale warning"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal warning update error=%v", err)
	}
	if due, _ := db.RunnerSubmissionRetryDue(ctx, dl.ID, dl.GenerationKey, now.Add(24*time.Hour)); due {
		t.Fatal("terminal request remained retryable")
	}
}

func TestDeliveryReceiptPageRejectsUnboundedOrUnscopedQueries(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	for name, args := range map[string]struct {
		downloadID, beforeID int64
		limit                int
	}{
		"zero download":      {0, 0, 1},
		"zero page size":     {11, 0, 0},
		"oversized page":     {11, 0, 101},
		"negative page size": {11, 0, -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := db.DeliveryReceiptPageForDownload(ctx, args.downloadID, args.beforeID, args.limit); err == nil {
				t.Fatalf("invalid receipt page query accepted: %+v", args)
			}
		})
	}
	page, next, err := db.DeliveryReceiptPageForDownload(ctx, 8888, 0, 10)
	if err != nil || len(page) != 0 || next != 0 {
		t.Fatalf("empty scoped receipt page=%+v next=%d err=%v", page, next, err)
	}
}
