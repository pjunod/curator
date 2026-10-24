package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestReplacementWorkerKeepsLostAddAcknowledgementAsExactRetry(t *testing.T) {
	base := &fakeClient{}
	client := &runnerSubmissionRetryClient{fakeClient: base, firstErr: errors.New("download client temporarily unavailable")}
	svc, db, itemID := setup(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-ADMISSION-RETRY", 25)}, base)
	ctx := context.Background()
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "worker-client-admission-retry", RequestID: "worker-client-admission-retry", Fingerprint: "worker-client-admission-retry-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "episodes", MediaItemID: itemID,
		EpisodeIDs: []int64{item.Seasons[0].Episodes[0].ID}, ReasonCode: "bad_media", Reason: "retry an accepted release after client outage", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatalf("candidate admission outage should remain a durable in-flight attempt: %v", err)
	}
	waiting, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || waiting.State != "replacing" {
		t.Fatalf("client outage did not preserve the exact replacement attempt: operation=%+v err=%v", waiting, err)
	}
	if len(client.urls) != 1 {
		t.Fatalf("worker should make one admission attempt and suppress the same candidate's pack fallback: %v", client.added)
	}
	rows, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 1 || rows[0].State != "grabbed" || rows[0].SubmissionPhase != "uncertain" || rows[0].Transfer == "" || rows[0].ExecutionPayload == "" {
		t.Fatalf("lost Add acknowledgement did not retain one exact replayable attempt: rows=%+v err=%v", rows, err)
	}

	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET runner_submission_retry_at=0 WHERE id=? AND generation_key=?`, rows[0].ID, rows[0].GenerationKey); err != nil {
		t.Fatal(err)
	}
	svc.processRunnerSubmissionRetries(ctx)
	rows, err = db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 1 || rows[0].SubmissionPhase != "submitted" || rows[0].Handle == "" {
		t.Fatalf("Runner submission retry did not bind the accepted exact attempt: rows=%+v err=%v", rows, err)
	}
	if err := svc.processReplacementOperation(ctx, waiting); err != nil {
		t.Fatalf("replacement worker did not continue to honor the accepted active attempt: %v", err)
	}
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "replacing" {
		t.Fatalf("replacement operation left its active state after submission recovery: operation=%+v err=%v", latest, err)
	}
	if len(client.urls) != 2 {
		t.Fatalf("exact submission retry or worker suppression made an unexpected number of client calls: %v", client.urls)
	}
}
