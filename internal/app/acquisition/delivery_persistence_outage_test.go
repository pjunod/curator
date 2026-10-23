package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDiscoveryInsertFailureLeavesPageReplayable(t *testing.T) {
	ctx := context.Background()
	base := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, base)
	if err := db.SetDeliveryConflict(ctx, receiptIDFor(t, db, dl.ID), "isolate discovery fixture"); err != nil {
		t.Fatal(err)
	}
	delivery := unassociatedRunnerDelivery(3, "insert-outage", "job-insert", "transfer-insert")
	protocol := &scriptedDeliveryImportClient{
		deliveryImportClient: base,
		pages: map[uint64]ports.RunnerDeliveryPage{
			0: {ProtocolVersion: 1, Deliveries: []ports.RunnerDelivery{delivery}, NextCursor: 3},
			3: {ProtocolVersion: 1, NextCursor: 3},
		},
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_discovery_receipt BEFORE INSERT ON runner_delivery_receipts WHEN NEW.delivery_id='insert-outage' BEGIN SELECT RAISE(ABORT, 'injected receipt journal outage'); END`); err != nil {
		t.Fatal(err)
	}
	svc.discoverRunnerDeliveries(ctx, cfg, protocol)
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_receipts WHERE client_id=? AND delivery_id='insert-outage'`, cfg.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	cursor, err := db.DeliveryCursor(ctx, cfg.ID)
	if err != nil || count != 0 || cursor != 0 || base.accepts != 0 {
		t.Fatalf("failed journal insert was acknowledged or skipped: rows=%d cursor=%d accepts=%d err=%v", count, cursor, base.accepts, err)
	}
	if len(protocol.after) != 1 || protocol.after[0] != 0 {
		t.Fatalf("discovery continued after journal failure: cursors=%v", protocol.after)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_discovery_receipt`); err != nil {
		t.Fatal(err)
	}
	protocol.after = nil
	svc.discoverRunnerDeliveries(ctx, cfg, protocol)
	if len(protocol.after) != 2 || protocol.after[0] != 0 || protocol.after[1] != 3 {
		t.Fatalf("discovery did not replay the page from the durable cursor: cursors=%v", protocol.after)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_receipts WHERE client_id=? AND delivery_id='insert-outage'`, cfg.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	cursor, err = db.DeliveryCursor(ctx, cfg.ID)
	if err != nil || count != 1 || cursor != 0 {
		t.Fatalf("replayed journal/cursor state rows=%d cursor=%d err=%v", count, cursor, err)
	}
}

func TestReceiptSchedulerWaitsForExactAssociationCommitAfterDatabaseFailure(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	if err := db.SetDeliveryConflict(ctx, receiptIDFor(t, db, dl.ID), "isolate association fixture"); err != nil {
		t.Fatal(err)
	}
	delivery := unassociatedRunnerDelivery(2, "association-outage", dl.Handle, dl.Transfer)
	publication := filepath.Join(filepath.Dir(dl.ImportPath), "association-outage")
	receipt, err := db.SaveDiscoveredDelivery(ctx, cfg.ID, delivery, publication)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_delivery_association BEFORE UPDATE OF download_id ON runner_delivery_receipts WHEN NEW.delivery_id='association-outage' AND NEW.download_id!=0 BEGIN SELECT RAISE(ABORT, 'injected association journal outage'); END`); err != nil {
		t.Fatal(err)
	}
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	blocked, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.DownloadID != 0 || blocked.GenerationKey != "" || protocol.accepts != 0 || svc.ImportRunning(dl.ID) {
		t.Fatalf("scheduler acted before exact association committed: receipt=%+v accepts=%d running=%v", blocked, protocol.accepts, svc.ImportRunning(dl.ID))
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_delivery_association`); err != nil {
		t.Fatal(err)
	}
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	associated, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if associated.DownloadID != dl.ID || associated.GenerationKey != dl.GenerationKey || associated.DownloadHandle != dl.Handle || associated.Delivery.RequestToken != dl.Transfer || associated.MediaItemID != dl.MediaItemID || associated.CopyID != dl.CopyID {
		t.Fatalf("recovered association did not retain exact download identity: %+v", associated)
	}
	if protocol.accepts != 0 {
		t.Fatalf("association recovery acknowledged without a verified import: accepts=%d", protocol.accepts)
	}
}

func TestDeliveryImportDoesNotMutateLibraryWhenAcceptancePrepareCannotPersist(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_delivery_acceptance_prepare BEFORE UPDATE OF ack_state ON runner_delivery_receipts WHEN OLD.ack_state='not_accepted' AND NEW.ack_state='pending' BEGIN SELECT RAISE(ABORT, 'injected acceptance prepare outage'); END`); err != nil {
		t.Fatal(err)
	}
	err := svc.runDeliveryImport(ctx, dl, cfg, receiptID)
	if err == nil || !strings.Contains(err.Error(), "injected acceptance prepare outage") {
		t.Fatalf("failed durable acceptance prepare returned %v", err)
	}
	blocked, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State == "imported" || blocked.AckState != "not_accepted" || blocked.ConsumerReceiptID != "" || protocol.accepts != 0 {
		t.Fatalf("remote or local mutation occurred without durable acceptance intent: receipt=%+v accepts=%d", blocked, protocol.accepts)
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); err != nil {
		t.Fatalf("verified publication was not retained for retry: %v", err)
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Files) != 0 {
		t.Fatalf("library changed despite failed acceptance preparation: files=%+v err=%v", item.Files, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_delivery_acceptance_prepare`); err != nil {
		t.Fatal(err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("exact receipt did not recover after durable prepare became available: %v", err)
	}
	completed, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || completed.State != "imported" || completed.AckState != "accepted" || protocol.accepts != 1 {
		t.Fatalf("recovered delivery state=%+v accepts=%d err=%v", completed, protocol.accepts, err)
	}
	item, err = db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Files) != 1 {
		t.Fatalf("recovered delivery did not place exactly once: files=%+v err=%v", item.Files, err)
	}
}

type outageDeliveryLookupClient struct {
	*deliveryImportClient
	getErr error
}

func (c *outageDeliveryLookupClient) GetDelivery(_ context.Context, id string) (ports.RunnerDelivery, error) {
	c.getDeliveryCalls++
	if c.getErr != nil {
		return ports.RunnerDelivery{}, c.getErr
	}
	if id != c.lookup.DeliveryID {
		return ports.RunnerDelivery{}, errors.New("unexpected delivery identity")
	}
	return c.lookup, nil
}

func TestRunnerSourceCleanupGETOutagePersistsBackoffAcrossRestart(t *testing.T) {
	ctx := context.Background()
	base := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, base)
	lookup := &outageDeliveryLookupClient{deliveryImportClient: base}
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return lookup }
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted import: %v", err)
	}
	before, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || before.State != "imported" || before.AckState != "accepted" {
		t.Fatalf("fixture did not reach accepted import: receipt=%+v err=%v", before, err)
	}
	lookup.lookup = before.Delivery
	lookup.lookup.SourceCleanup = []byte(`{"status":"succeeded","attempts":2}`)
	lookup.getErr = errors.New("Runner lookup temporarily unavailable")
	if err := svc.refreshOneDeliverySourceCleanup(ctx, lookup, before); err == nil {
		t.Fatal("Runner GET outage was not surfaced to the refresh worker")
	}
	if lookup.getDeliveryCalls != 1 {
		t.Fatalf("initial source refresh GET calls=%d, want 1", lookup.getDeliveryCalls)
	}
	message, due, err := db.DeliverySourceRefreshError(ctx, receiptID)
	if err != nil || !strings.Contains(message, "temporarily unavailable") || !due.After(time.Now()) {
		t.Fatalf("GET outage did not persist visible backoff state: error=%q due=%v err=%v", message, due, err)
	}
	if err := svc.refreshOneDeliverySourceCleanup(ctx, lookup, before); err != nil {
		t.Fatalf("immediate replay should be skipped by the persisted deadline: %v", err)
	}
	if lookup.getDeliveryCalls != 1 {
		t.Fatalf("immediate source refresh bypassed backoff: GET calls=%d", lookup.getDeliveryCalls)
	}

	dbDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dueRows, err := reopened.DeliveryReceiptsNeedingSourceRefresh(ctx, cfg.ID, 4)
	if err != nil || len(dueRows) != 0 {
		t.Fatalf("restart forgot the future source refresh deadline: rows=%+v err=%v", dueRows, err)
	}
	if _, err := reopened.W.ExecContext(ctx, `UPDATE runner_delivery_source_refresh SET next_attempt_at=0 WHERE receipt_id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	lookup.getErr = nil
	lookup.lookup.SourceCleanup = []byte(`{"status":"succeeded","attempts":3}`)
	restarted := New(reopened, nil, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return lookup })
	restarted.refreshDeliverySourceCleanup(ctx, cfg, lookup)
	after, err := reopened.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || string(after.Delivery.SourceCleanup) != string(lookup.lookup.SourceCleanup) {
		t.Fatalf("successful post-restart GET did not refresh mutable cleanup state: source=%s err=%v", after.Delivery.SourceCleanup, err)
	}
	if after.Delivery.PublicationGeneration != before.Delivery.PublicationGeneration || after.Delivery.ManifestDigest != before.Delivery.ManifestDigest || after.State != "imported" {
		t.Fatalf("source refresh changed immutable receipt authority: before=%+v after=%+v", before, after)
	}
	if lookup.getDeliveryCalls != 2 {
		t.Fatalf("recovered source cleanup GET calls=%d, want exactly one post-restart lookup", lookup.getDeliveryCalls)
	}
}
