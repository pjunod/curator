package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestManualAdmissionWorkerPersistsLongBlockedWarningWithoutResubmitting(t *testing.T) {
	client := &fakeClient{addErr: errors.New("lost client acknowledgement")}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	req := GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL",
		DownloadURL: "magnet:manual-warning", Indexer: "idx", Protocol: "torrent", RequestID: "manual-warning-request",
	}
	id, err := svc.GrabManual(ctx, req)
	if err == nil {
		t.Fatal("lost acknowledgement should leave the manual request uncertain")
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET manual_retry_at=0,manual_blocked_at=?,error='old transient warning' WHERE id=?`, time.Now().Add(-31*time.Minute).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	svc.startManualAdmissionWorker(serviceCtx)
	deadline := time.Now().Add(3 * time.Second)
	var currentErr string
	for time.Now().Before(deadline) {
		row, loadErr := db.GetDownload(ctx, id)
		if loadErr != nil {
			cancel()
			svc.importWG.Wait()
			t.Fatal(loadErr)
		}
		currentErr = row.Error
		if strings.Contains(currentErr, "blocked for at least 30 minutes") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	svc.importWG.Wait()
	if !strings.Contains(currentErr, "blocked for at least 30 minutes") || !strings.Contains(currentErr, "retry remains queued") {
		t.Fatalf("manual admission worker did not persist the long-blocked warning: %q", currentErr)
	}
	row, err := db.GetDownload(ctx, id)
	if err != nil || row.SubmissionPhase != "uncertain" || row.State != "grabbed" {
		t.Fatalf("warning changed the uncertain request's retryable state: row=%+v err=%v", row, err)
	}
	if len(client.added) != 1 {
		t.Fatalf("uncertain request was resubmitted while warning: adds=%v", client.added)
	}
}

func TestRefreshQueueWarnsForLongMissingExactTransferWithoutRetryingAdd(t *testing.T) {
	client := &handlelessTransferClient{}
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	req := GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1, Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL",
		DownloadURL: "magnet:exact-transfer-warning", Indexer: "idx", Protocol: "torrent", RequestID: "exact-transfer-warning-request",
	}
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := db.GetDownload(ctx, id)
	if err != nil || accepted.SubmissionPhase != "submitted" || accepted.Handle != "" || !accepted.ExactTransferCorrelation || accepted.Transfer == "" {
		t.Fatalf("handleless acceptance fixture=%+v err=%v", accepted, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET updated_at=? WHERE id=? AND generation_key=?`, time.Now().Add(-31*time.Minute).UnixMilli(), id, accepted.GenerationKey); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	warned, err := db.GetDownload(ctx, id)
	if err != nil || !strings.Contains(warned.Error, "has not appeared with its exact transfer token for at least 30 minutes") {
		t.Fatalf("queue refresh did not persist the exact-token wait warning: row=%+v err=%v", warned, err)
	}
	if warned.Handle != "" || warned.Transfer != accepted.Transfer || warned.GenerationKey != accepted.GenerationKey || warned.SubmissionPhase != "submitted" {
		t.Fatalf("warning altered the accepted transfer identity: before=%+v after=%+v", accepted, warned)
	}
	if len(client.added) != 1 {
		t.Fatalf("exact-token status wait caused another Add: %v", client.added)
	}
}
