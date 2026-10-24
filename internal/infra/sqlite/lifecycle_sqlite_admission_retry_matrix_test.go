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

func TestRunnerAdmissionPollRecoveryAndInstallationBindingRejectReusedIdentity(t *testing.T) {
	ctx := context.Background()
	db, dir := openLifecycleCoverageDB(t)
	item := insertLifecycleAdmission(t, db, "Poll recovery", "poll-recovery-token", "uncertain", "automatic", false)
	other := insertLifecycleAdmission(t, db, "Other client", "poll-other-token", "uncertain", "automatic", false)
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET client_id=72 WHERE id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_900_000_000, 0)
	for name, args := range map[string]struct {
		id         int64
		generation string
	}{
		"missing id":         {0, item.GenerationKey},
		"missing generation": {item.ID, ""},
		"wrong generation":   {item.ID, "other-generation"},
	} {
		t.Run(name, func(t *testing.T) {
			if claimed, err := db.ClaimRunnerAdmissionPoll(ctx, args.id, args.generation, now); err == nil || claimed {
				t.Fatalf("invalid poll claim claimed=%v err=%v", claimed, err)
			}
		})
	}
	claimed, err := db.ClaimRunnerAdmissionPoll(ctx, item.ID, item.GenerationKey, now)
	if err != nil || !claimed {
		t.Fatalf("first poll claimed=%v err=%v", claimed, err)
	}
	if err := db.RecordRunnerAdmissionPollResult(ctx, item.ID, item.GenerationKey, false, errors.New("daemon unavailable"), now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRunnerAdmissionPoll(ctx, item.ID, item.GenerationKey, now.Add(29*time.Second)); err != nil || claimed {
		t.Fatalf("poll bypassed persisted retry deadline: claimed=%v err=%v", claimed, err)
	}
	if err := db.RecordRunnerAdmissionPollResult(ctx, item.ID, item.GenerationKey, true, nil, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRunnerAdmissionPoll(ctx, item.ID, item.GenerationKey, now.Add(5*time.Minute+30*time.Second)); err != nil || !claimed {
		t.Fatalf("recovered poll did not resume after successful-read pace: claimed=%v err=%v", claimed, err)
	}
	if err := db.BindRunnerAdmission(ctx, item.ID, item.GenerationKey, item.Transfer, "runner-install-1", "admission-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRunnerAdmission(ctx, item.ID, item.GenerationKey, item.Transfer, "runner-install-1", "admission-1"); err != nil {
		t.Fatalf("binding replay failed: %v", err)
	}
	if err := db.BindRunnerAdmission(ctx, item.ID, item.GenerationKey, item.Transfer, "runner-install-2", "admission-1"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("installation rebind error=%v", err)
	}
	if err := db.BindRunnerAdmission(ctx, item.ID, item.GenerationKey, item.Transfer, "runner-install-1", "admission-2"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("admission generation rebind error=%v", err)
	}
	order, err := db.RunnerAdmissionPollOrder(ctx, 71)
	if err != nil || len(order) != 1 || order[0] != item.ID {
		t.Fatalf("poll order crossed client scope: ids=%v err=%v", order, err)
	}
}

func TestProtectedDeliveryFileRetryBackoffAndExplicitRemovalAreSeparate(t *testing.T) {
	db, ctx, receipt, batch := explicitDeleteFixture(t)
	defer func() { _ = db.Close() }()
	manifest := receipt.Delivery.Files[0]
	file := DeliveryManifestFile{Path: manifest.Path, SizeBytes: manifest.SizeBytes, SHA256: manifest.SHA256, FileGeneration: "journal-file-generation"}
	if err := db.UpdateDeliveryFileDisposition(ctx, receipt.ID, file.Path, "unknown_recoverable", "complete candidate awaits a concrete handler", ""); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]struct {
		id   int64
		path string
	}{
		"missing receipt": {0, file.Path},
		"missing path":    {receipt.ID, ""},
		"missing outcome": {receipt.ID, "other.mkv"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.ScheduleProtectedDeliveryFileRetry(ctx, args.id, args.path, "recheck later"); err == nil {
				t.Fatalf("invalid protected-file retry accepted: %+v", args)
			}
		})
	}
	for attempt, delay := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		started := time.Now()
		if err := db.ScheduleProtectedDeliveryFileRetry(ctx, receipt.ID, file.Path, fmt.Sprintf("recovery evidence wait %d", attempt+1)); err != nil {
			t.Fatal(err)
		}
		outcome, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, file.Path)
		if err != nil || outcome.Disposition != "unknown_recoverable" || outcome.CleanupState != "protected" || outcome.CleanupAttempts != attempt+1 || outcome.CleanupBlockedSince.IsZero() {
			t.Fatalf("protected retry %d lost retained-source authority: outcome=%+v err=%v", attempt+1, outcome, err)
		}
		if remaining := outcome.CleanupNextAttemptAt.Sub(started); remaining < delay-time.Second || remaining > delay+time.Second {
			t.Fatalf("protected retry %d deadline delta=%v want approximately %v", attempt+1, remaining, delay)
		}
	}
	if err := db.SetDeliveryFileCleanup(ctx, receipt.ID, file.Path, "removed", "stale worker"); !errors.Is(err, ErrDeliveryReceiptStale) {
		t.Fatalf("ordinary cleanup retired positively recoverable bytes: %v", err)
	}
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatalf("exact operator removal intent was not journaled: %v", err)
	}
	if err := db.CompleteExplicitDeliveryFileDelete(ctx, batch.ID, receipt.ID, file.Path, "explicit_delivery_file_removed_retry_matrix", 0, "release", map[string]string{"path": file.Path}); err != nil {
		t.Fatalf("exact operator removal transaction failed: %v", err)
	}
	removed, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, file.Path)
	if err != nil || removed.CleanupState != "removed" || removed.CleanupError != "operator explicitly selected this manifest file" {
		t.Fatalf("operator removal did not settle exact selected file: %+v err=%v", removed, err)
	}
	stored, err := db.ExplicitDeliveryDeleteBatch(ctx, batch.ID)
	if err != nil || len(stored.Files) != 1 || !stored.Files[0].HistoryRecorded || stored.Files[0].CompletedAt.IsZero() {
		t.Fatalf("operator removal did not persist its exact journal audit: batch=%+v err=%v", stored, err)
	}
	var historyCount int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='explicit_delivery_file_removed_retry_matrix'`).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("operator removal history count=%d err=%v, want one", historyCount, err)
	}
	if err := db.ScheduleProtectedDeliveryFileRetry(ctx, receipt.ID, file.Path, "stale retry"); !errors.Is(err, ErrDeliveryReceiptStale) {
		t.Fatalf("retry resurrected an explicitly removed file: %v", err)
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
