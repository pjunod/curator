package acquisition

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type scriptedDeliveryImportClient struct {
	*deliveryImportClient
	pages map[uint64]ports.RunnerDeliveryPage
	after []uint64
}

func (c *scriptedDeliveryImportClient) ListDeliveries(_ context.Context, after uint64, _ int) (ports.RunnerDeliveryPage, error) {
	c.after = append(c.after, after)
	page, ok := c.pages[after]
	if !ok {
		return ports.RunnerDeliveryPage{}, fmt.Errorf("unexpected cursor %d", after)
	}
	return page, nil
}

func TestDiscoveryJournalsEveryPageBeforeCursorAndReplaysAfterCursorWriteFailure(t *testing.T) {
	ctx := context.Background()
	base := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, base)
	if err := db.SetDeliveryConflict(ctx, receiptIDFor(t, db, dl.ID), "unrelated setup receipt"); err != nil {
		t.Fatal(err)
	}
	first := unassociatedRunnerDelivery(2, "discovery-first", "job-201", "transfer-201")
	last := unassociatedRunnerDelivery(4, "discovery-last", "job-204", "transfer-204")
	protocol := &scriptedDeliveryImportClient{
		deliveryImportClient: base,
		pages: map[uint64]ports.RunnerDeliveryPage{
			0: {ProtocolVersion: 1, Deliveries: []ports.RunnerDelivery{first}, NextCursor: 2},
			// The cursor includes a scanned sequence whose row is already accepted
			// and therefore omitted from the pending-deliveries response.
			2: {ProtocolVersion: 1, Deliveries: []ports.RunnerDelivery{last}, NextCursor: 4},
			4: {ProtocolVersion: 1, NextCursor: 4},
		},
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_delivery_cursor_insert BEFORE INSERT ON runner_delivery_cursors BEGIN SELECT RAISE(ABORT, 'injected cursor write failure'); END`); err != nil {
		t.Fatal(err)
	}
	svc.discoverRunnerDeliveries(ctx, cfg, protocol)
	cursor, err := db.DeliveryCursor(ctx, cfg.ID)
	if err != nil || cursor != 0 {
		t.Fatalf("cursor advanced despite failed durable cursor write: cursor=%d err=%v", cursor, err)
	}
	assertDiscoveredDeliveryCount(t, ctx, db, cfg.ID, 1)
	if len(protocol.after) != 1 || protocol.after[0] != 0 {
		t.Fatalf("first sweep advanced past a page whose cursor could not be persisted: cursors=%v", protocol.after)
	}

	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_delivery_cursor_insert`); err != nil {
		t.Fatal(err)
	}
	protocol.after = nil
	svc.discoverRunnerDeliveries(ctx, cfg, protocol)
	if len(protocol.after) != 3 || protocol.after[0] != 0 || protocol.after[1] != 2 || protocol.after[2] != 4 {
		t.Fatalf("replay did not resume from the durable cursor and scan through the accepted gap: cursors=%v", protocol.after)
	}
	cursor, err = db.DeliveryCursor(ctx, cfg.ID)
	if err != nil || cursor != 0 {
		t.Fatalf("completed discovery sweep did not wrap its cursor: cursor=%d err=%v", cursor, err)
	}
	assertDiscoveredDeliveryCount(t, ctx, db, cfg.ID, 2)
}

func TestSourceCleanupRefreshTurnsChangedImmutableIdentityIntoConflict(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare locally imported delivery: %v", err)
	}
	before, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || before.State != "imported" || before.AckState != "accepted" {
		t.Fatalf("fixture did not reach accepted import: receipt=%+v err=%v", before, err)
	}
	selected := before
	protocol.lookup = before.Delivery
	protocol.lookup.PublicationGeneration = "different-publication-generation"
	protocol.lookup.SourceCleanup = []byte(`{"status":"succeeded","attempts":1}`)

	if err := svc.refreshOneDeliverySourceCleanup(ctx, protocol, selected); err != nil {
		t.Fatalf("identity change should be recorded as conflict, not a poll error: %v", err)
	}
	after, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "conflict" || after.AckState != before.AckState || !bytes.Equal(after.Delivery.SourceCleanup, before.Delivery.SourceCleanup) {
		t.Fatalf("changed Runner publication identity was not fenced before refreshing mutable progress: before=%+v after=%+v", before, after)
	}
	if protocol.getDeliveryCalls != 1 {
		t.Fatalf("source identity check made %d Runner lookups, want exactly one", protocol.getDeliveryCalls)
	}
}

func TestReceiptSchedulerBlocksOnlyOnAuthoritativeAckConflictAfterLocalImport(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{failCount: 1}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("transient Runner outage should not block verified local import: %v", err)
	}
	before, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || before.State != "imported" || before.AckState != "pending" {
		t.Fatalf("transient ACK did not leave durable local import and retry: receipt=%+v err=%v", before, err)
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Files) != 1 {
		t.Fatalf("local placement missing while ACK is pending: files=%+v err=%v", item.Files, err)
	}
	libraryPath := item.Files[0].Path
	if err := db.ForceDeliveryRetryNow(ctx, receiptID); err != nil {
		t.Fatal(err)
	}
	protocol.conflict = true
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	after, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || after.State != "conflict" {
		t.Fatalf("authoritative ACK conflict was not persisted: receipt=%+v err=%v", after, err)
	}
	if protocol.accepts != 2 {
		t.Fatalf("scheduler sent %d acceptance requests, want initial transient failure plus one conflict", protocol.accepts)
	}
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("conflict handling removed the already committed local media: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); err != nil {
		t.Fatalf("conflict handling removed the completed publication: %v", err)
	}
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	if protocol.accepts != 2 {
		t.Fatalf("conflicted receipt was retried automatically: acceptance calls=%d", protocol.accepts)
	}
}

func TestRefreshQueueAfterRestartRetriesDueDeliveryAcceptance(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{failCount: 1}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("transient ACK outage should permit local import: %v", err)
	}
	before, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || before.State != "imported" || before.AckState != "pending" {
		t.Fatalf("initial attempt did not persist locally imported pending receipt: %+v err=%v", before, err)
	}
	dataDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ForceDeliveryRetryNow(ctx, receiptID); err != nil {
		t.Fatal(err)
	}
	protocol.failCount = 0
	restarted := New(reopened, nil, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	if err := restarted.RefreshQueue(ctx); err != nil {
		t.Fatalf("normal queue refresh did not recover the durable delivery receipt: %v", err)
	}
	after, err := reopened.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || after.State != "imported" || after.AckState != "accepted" {
		t.Fatalf("startup queue sweep did not finish due acceptance: %+v err=%v", after, err)
	}
	if protocol.accepts != 2 {
		t.Fatalf("startup queue sweep acceptance calls=%d, want one failed pre-restart attempt and one recovery", protocol.accepts)
	}
	item, err := reopened.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Files) != 1 {
		t.Fatalf("startup ACK retry duplicated or lost local placement: files=%+v err=%v", item.Files, err)
	}
}

func TestDiscoveryLeavesCursorUnchangedWhenRunnerLookupFails(t *testing.T) {
	ctx := context.Background()
	base := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, base)
	if err := db.SetDeliveryConflict(ctx, receiptIDFor(t, db, dl.ID), "unrelated setup receipt"); err != nil {
		t.Fatal(err)
	}
	protocol := &failingDeliveryListClient{deliveryImportClient: base, err: errors.New("Runner unavailable")}
	svc.discoverRunnerDeliveries(ctx, cfg, protocol)
	cursor, err := db.DeliveryCursor(ctx, cfg.ID)
	if err != nil || cursor != 0 {
		t.Fatalf("failed discovery changed cursor: cursor=%d err=%v", cursor, err)
	}
	if protocol.calls != 1 {
		t.Fatalf("discovery continued after shared Runner outage: calls=%d", protocol.calls)
	}
}

type failingDeliveryListClient struct {
	*deliveryImportClient
	err   error
	calls int
}

func (c *failingDeliveryListClient) ListDeliveries(context.Context, uint64, int) (ports.RunnerDeliveryPage, error) {
	c.calls++
	return ports.RunnerDeliveryPage{}, c.err
}

func unassociatedRunnerDelivery(sequence uint64, id, jobID, token string) ports.RunnerDelivery {
	content := []byte("video")
	files := []ports.RunnerFile{{Path: "episode.mkv", SizeBytes: int64(len(content)), SHA256: sha256Hex(content)}}
	digest, _ := runnerManifestDigest(files)
	return ports.RunnerDelivery{
		ProtocolVersion: 1, Sequence: sequence, DeliveryID: id,
		PublicationGeneration: "publication-" + id, ArtifactID: "artifact-" + id,
		JobID: jobID, RequestToken: token, RunnerInstanceID: "runner-1",
		PublicationPath: "/remote/completed/" + id, ManifestDigest: digest,
		Files: files, TerminalOutcome: "SUCCESS", ReadyAt: time.Now().UTC(),
	}
}

func assertDiscoveredDeliveryCount(t *testing.T, ctx context.Context, db *sqlite.DB, clientID int64, want int) {
	t.Helper()
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_receipts WHERE client_id=? AND delivery_id IN ('discovery-first','discovery-last')`, clientID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("journal has %d unique discovery rows, want %d", count, want)
	}
}
