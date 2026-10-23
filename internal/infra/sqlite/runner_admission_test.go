package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestRunnerAdmissionIsPinnedToDownloadAndInstallation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Admission pin", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ReleaseTitle: "release", Protocol: "nzb", ClientID: 7, State: "grabbed", Transfer: "token-1", RequestTokenIdempotent: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "runner-A", "admission-A"); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "runner-A", "admission-A"); err != nil {
		t.Fatalf("exact replay rejected: %v", err)
	}
	for _, mismatch := range []struct{ runner, generation string }{{"runner-B", "admission-A"}, {"runner-A", "admission-B"}} {
		if err := db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, mismatch.runner, mismatch.generation); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
			t.Fatalf("mismatching admission %v returned %v", mismatch, err)
		}
	}
	if err := db.BindRunnerAdmission(ctx, dl.ID, "stale-generation", dl.Transfer, "runner-A", "admission-A"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("stale download generation returned %v", err)
	}
}

func TestRunnerSubmissionRetryHonorsProviderDeadlineAndCancellation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "retry deadline", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ReleaseTitle: "release", Protocol: "nzb", ClientID: 7, State: "grabbed", Transfer: "deadline-token", RequestTokenIdempotent: true, SubmissionPhase: "uncertain"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_900_000_000, 0)
	provider := now.Add(20 * time.Minute)
	if _, err = db.ScheduleRunnerSubmissionRetry(ctx, dl.ID, dl.GenerationKey, now, "rate limited", provider); err != nil {
		t.Fatal(err)
	}
	var due int64
	if err = db.R.QueryRowContext(ctx, `SELECT runner_submission_retry_at FROM downloads WHERE id=?`, dl.ID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if got := time.UnixMilli(due); !got.Equal(provider) {
		t.Fatalf("retry due=%v want provider deadline %v", got, provider)
	}
	if err = db.ParkDownload(ctx, dl.ID, "operator cancellation; review"); err != nil {
		t.Fatal(err)
	}
	retries, err := db.ListRunnerSubmissionRetries(ctx, provider.Add(time.Hour), 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(retries) != 0 {
		t.Fatalf("cancelled retry remained runnable: %+v", retries)
	}
}

func TestAutomaticSubmissionRetryLimitExcludesOlderManualRows(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Retry fairness", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		_, err := db.InsertDownloadRecord(ctx, Download{
			MediaItemID: itemID, ReleaseTitle: "older manual", Protocol: "nzb", ClientID: 7,
			State: "grabbed", Transfer: fmt.Sprintf("manual-token-%02d", i), Intent: "manual_additional",
			RequestTokenIdempotent: true, SubmissionPhase: "uncertain",
		})
		if err != nil {
			t.Fatalf("insert manual retry %d: %v", i, err)
		}
	}
	automatic, err := db.InsertDownloadRecord(ctx, Download{
		MediaItemID: itemID, ReleaseTitle: "automatic", Protocol: "nzb", ClientID: 7,
		State: "grabbed", Transfer: "automatic-token", Intent: "automatic",
		RequestTokenIdempotent: true, SubmissionPhase: "uncertain",
	})
	if err != nil {
		t.Fatal(err)
	}
	retries, err := db.ListRunnerSubmissionRetries(ctx, time.Now(), 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(retries) != 1 || retries[0].DownloadID != automatic.ID {
		t.Fatalf("automatic retry selection starved by manual rows: %+v, want only download %d", retries, automatic.ID)
	}
	if due, err := db.RunnerSubmissionRetryDue(ctx, automatic.ID, automatic.GenerationKey, time.Now()); err != nil || !due {
		t.Fatalf("automatic retry due=%v err=%v", due, err)
	}
}

func TestRunnerAdmissionPollScheduleIsDurableAndBacksOff(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Admission schedule", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ReleaseTitle: "release", Protocol: "nzb", ClientID: 7, State: "grabbed", Transfer: "token-schedule", RequestTokenIdempotent: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_900_000_000, 0)
	for attempt, interval := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute} {
		claimed, err := db.ClaimRunnerAdmissionPoll(ctx, dl.ID, dl.GenerationKey, now)
		if err != nil || !claimed {
			t.Fatalf("attempt %d claimed=%v err=%v", attempt, claimed, err)
		}
		if err = db.RecordRunnerAdmissionPollResult(ctx, dl.ID, dl.GenerationKey, false, nil, now); err != nil {
			t.Fatal(err)
		}
		if claimed, err = db.ClaimRunnerAdmissionPoll(ctx, dl.ID, dl.GenerationKey, now.Add(interval-time.Millisecond)); err != nil || claimed {
			t.Fatalf("attempt %d was claimed before backoff: claimed=%v err=%v", attempt, claimed, err)
		}
		now = now.Add(interval)
	}
	if err = db.RecordRunnerAdmissionPollResult(ctx, dl.ID, dl.GenerationKey, true, nil, now); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRunnerAdmissionPoll(ctx, dl.ID, dl.GenerationKey, now.Add(5*time.Minute-time.Millisecond)); err != nil || claimed {
		t.Fatalf("successful lookup was not paced: claimed=%v err=%v", claimed, err)
	}
}
