package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withSettledFiles treats every file as already settled, so a test can
// exercise the memo without waiting out the racy margin.
func withSettledFiles(t *testing.T) {
	t.Helper()
	previous := digestMemoRacyMargin.Load()
	digestMemoRacyMargin.Store(int64(-time.Second))
	t.Cleanup(func() { digestMemoRacyMargin.Store(previous) })
}

func TestDigestMemoReadsAStableFileOnce(t *testing.T) {
	withSettledFiles(t)
	path := filepath.Join(t.TempDir(), "payload.mkv")
	if err := os.WriteFile(path, []byte("the same bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := digestFullReads.Load()
	first, _, err := fileDigest(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := fileDigest(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("memo changed the digest: %s then %s", first, second)
	}
	if reads := digestFullReads.Load() - before; reads != 1 {
		t.Fatalf("a stable file was read %d times, want once", reads)
	}
}

// Same size, new bytes: the generation changes, so the memo must miss and
// the new digest must be computed from the new bytes.
func TestDigestMemoMissesARewrittenFile(t *testing.T) {
	withSettledFiles(t)
	path := filepath.Join(t.TempDir(), "payload.mkv")
	if err := os.WriteFile(path, []byte("original bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, _, err := fileDigest(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("corrupt bytes!"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _, err := fileDigest(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("a same-size rewrite returned the memoized digest of the old bytes")
	}
}

// A file whose last change is not older than the racy margin is still being
// settled: it is never memoized, so coarse timestamps cannot hide a rewrite.
func TestDigestMemoNeverRemembersARacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.mkv")
	if err := os.WriteFile(path, []byte("just written"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := digestFullReads.Load()
	for i := 0; i < 2; i++ {
		if _, _, err := fileDigest(context.Background(), path); err != nil {
			t.Fatal(err)
		}
	}
	if reads := digestFullReads.Load() - before; reads != 2 {
		t.Fatalf("a freshly changed file was read %d times, want 2 (no memo inside the racy margin)", reads)
	}
}

// The field cost: verify then recheck read the same payload twice per
// attempt, and every retry read it again. A settled payload is read once.
func TestDeliveryVerificationReadsAPayloadOnceAcrossAttempts(t *testing.T) {
	withSettledFiles(t)
	ctx := context.Background()
	svc, db, dl, cfg, _ := deliveryImportFixture(t, &deliveryImportClient{})
	r, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
	if err != nil {
		t.Fatal(err)
	}
	before := digestFullReads.Load()
	for attempt := 0; attempt < 3; attempt++ {
		files, err := svc.verifyDeliveryManifest(ctx, cfg, r)
		if err != nil {
			t.Fatalf("attempt %d verify: %v", attempt, err)
		}
		if err := svc.recheckDeliveryFiles(ctx, r, files); err != nil {
			t.Fatalf("attempt %d recheck: %v", attempt, err)
		}
	}
	if reads := digestFullReads.Load() - before; reads != 1 {
		t.Fatalf("three verify+recheck passes read the payload %d times, want once", reads)
	}
}

// A memo hit must not survive tampering between verification and recheck.
func TestDeliveryRecheckStillCatchesAChangedPayload(t *testing.T) {
	withSettledFiles(t)
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	r, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
	if err != nil {
		t.Fatal(err)
	}
	files, err := svc.verifyDeliveryManifest(ctx, cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(publication, epRelease+".mkv"), []byte("VIDEO"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.recheckDeliveryFiles(ctx, r, files); err == nil {
		t.Fatal("recheck accepted a payload rewritten after verification")
	}
}

// Cleanup work goes to its own worker and never occupies an import slot.
func TestPayloadCleanupNeverTakesAnImportSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, db, dl, cfg, _ := deliveryImportFixture(t, &deliveryImportClient{})
	r, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
	if err != nil {
		t.Fatal(err)
	}
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 1)
	svc.cleanupCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	if !svc.queueDeliveryCleanup(ctx, cfg, r) {
		t.Fatal("cleanup was not queued")
	}
	if len(svc.importCh) != 0 || len(svc.cleanupCh) != 1 {
		t.Fatalf("cleanup landed on the import queue: imports=%d cleanup=%d", len(svc.importCh), len(svc.cleanupCh))
	}
	job := <-svc.cleanupCh
	if job.cleanupCancel != nil {
		job.cleanupCancel()
	}
}
