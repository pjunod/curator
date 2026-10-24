package acquisition

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestManualHandlelessAcknowledgementPersistenceFailureRecoversAfterRestart(t *testing.T) {
	ctx := context.Background()
	client := &handlelessTransferClient{}
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	req := GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.MANUAL", DownloadURL: "magnet:handleless-retry",
		Indexer: "idx", Protocol: "torrent", RequestID: "manual-handleless-ack-retry",
	}
	id, err := svc.GrabManual(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := db.GetDownload(ctx, id)
	if err != nil || accepted.SubmissionPhase != "submitted" || accepted.Handle != "" || accepted.Transfer == "" {
		t.Fatalf("initial handleless acceptance=%+v err=%v", accepted, err)
	}
	client.statuses = []ports.DownloadStatus{{Handle: "native-handle-after-restart", Transfer: accepted.Transfer, State: ports.StateDownloading}}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_manual_handle_reconciliation BEFORE UPDATE OF handle ON downloads WHEN OLD.id=`+fmt.Sprint(id)+` AND NEW.handle<>'' BEGIN SELECT RAISE(ABORT,'injected handle reconciliation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.resumeManualRequest(ctx, id); err == nil {
		t.Fatal("matching accepted request passed an injected handle persistence failure")
	}
	stillAccepted, err := db.GetDownload(ctx, id)
	if err != nil || stillAccepted.SubmissionPhase != "submitted" || stillAccepted.Handle != "" || stillAccepted.Transfer != accepted.Transfer || stillAccepted.GenerationKey != accepted.GenerationKey {
		t.Fatalf("failed handle persistence changed accepted request custody: %+v err=%v", stillAccepted, err)
	}
	if len(client.added) != 1 {
		t.Fatalf("local acknowledgement recovery resubmitted accepted payload: %v", client.added)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_manual_handle_reconciliation`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fresh := New(reopened, nil, nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)
	if _, err := fresh.resumeManualRequest(ctx, id); err != nil {
		t.Fatalf("exact accepted transfer did not reconcile after restart: %v", err)
	}
	settled, err := reopened.GetDownload(ctx, id)
	if err != nil || settled.SubmissionPhase != "submitted" || settled.Handle != "native-handle-after-restart" || settled.Transfer != accepted.Transfer || settled.GenerationKey != accepted.GenerationKey {
		t.Fatalf("restarted reconciliation changed request identity: %+v err=%v", settled, err)
	}
	if len(client.added) != 1 {
		t.Fatalf("restart replayed an already accepted manual submission: %v", client.added)
	}
}
