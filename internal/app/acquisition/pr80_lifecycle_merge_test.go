package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestStorageChangeActionsShareWholeLibraryAuthority(t *testing.T) {
	for _, action := range []string{"preview", "accept"} {
		t.Run(action, func(t *testing.T) {
			svc := &Service{}
			_, releaseTitle := svc.lockImportTarget(context.Background(), 7)
			defer releaseTitle()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			request := StorageChangeRequest{Root: t.TempDir(), ExpectedIdentity: "old", ObservedIdentity: "new", ReviewToken: "reviewed"}
			var err error
			if action == "preview" {
				_, err = svc.PreviewCompletedStorageChange(ctx, request)
			} else {
				err = svc.AcceptCompletedStorageChange(ctx, request)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("storage %s crossed a held publication title: %v", action, err)
			}
			releaseTitle()
			_, releaseAll, err := svc.lockAllImportTargets(context.Background(), time.Second)
			if err != nil {
				t.Fatalf("storage %s leaked publication authority: %v", action, err)
			}
			releaseAll()
		})
	}
}

func TestIdenticalByteProofCannotAuthorizeLegacyBacklogCleanup(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatal(err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
	if err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("library files=%+v err=%v", files, err)
	}
	if err := db.SetFileQualityFrom(ctx, files[0].ID, files[0].Quality, mediainfo.ProvenanceImplausible, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	// Resolved rows are intentionally omitted by manifest reimport. Bind the
	// exact initial verification retained by the per-file ledger instead.
	verified := verifiedDeliveryFile{relative: row.Path, absolute: filepath.Join(publication, row.Path), size: row.SizeBytes, digest: row.SHA256, generation: row.FileGeneration}
	if _, _, ok := svc.deliveryRedundantLibraryProof(ctx, r, verified, ""); !ok {
		t.Fatal("fixture must exercise the new exact-byte proof despite implausible library quality")
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, publication); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err == nil || !strings.Contains(err.Error(), "legacy review") {
		t.Fatalf("exact bytes bypassed cleanup review: %v", err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, r, row); err == nil {
		t.Fatal("direct cleanup bypassed legacy review at unlink boundary")
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); err != nil {
		t.Fatalf("legacy payload removed: %v", err)
	}
	if _, err := os.Stat(row.LibraryPath); err != nil {
		t.Fatalf("library proof changed: %v", err)
	}
}

func TestAbandonDeliveryDeleteUsesCurrentTitleAuthority(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	r, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := svc.verifyDeliveryManifest(ctx, cfg, r)
	if err != nil || len(verified) != 1 {
		t.Fatalf("verify=%+v err=%v", verified, err)
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, []sqlite.DeliveryManifestFile{{Path: verified[0].relative, SizeBytes: verified[0].size, SHA256: verified[0].digest, FileGeneration: verified[0].generation}}, nil, false); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Lstat(verified[0].absolute)
	if err != nil {
		t.Fatal(err)
	}
	r.ConsumerReceiptID = "per-title-reviewed-delete"
	r.NextAttemptAt = time.Now()
	if err := db.PrepareDeliveryAcceptance(ctx, r); err != nil {
		t.Fatal(err)
	}
	dl.ImportPath, dl.SavePath = publication, r.Delivery.PublicationPath
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := completedEntryForPath(t, svc, publication)
	parent, err := os.Lstat(publication)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := observeStorageIdentity(publication)
	if err != nil {
		t.Fatal(err)
	}
	for _, sameTitle := range []bool{false, true} {
		id := "other-title-cancel"
		heldItem := dl.MediaItemID + 1000
		if sameTitle {
			id = "same-title-cancel"
			heldItem = dl.MediaItemID
		}
		batch := sqlite.ExplicitDeliveryDeleteBatch{ID: id, ReceiptID: r.ID, PayloadPath: publication, InventoryFingerprint: entry.Fingerprint, PublicationDirectoryIdentity: deliveryDirectoryObjectIdentity(parent), StorageIdentity: storage, GenerationKey: dl.GenerationKey, RequestToken: r.Delivery.RequestToken, PublicationGeneration: r.Delivery.PublicationGeneration, Files: []sqlite.ExplicitDeliveryDeleteFile{{Path: verified[0].relative, ReviewedPath: verified[0].absolute, SnapshotIdentity: completedReviewFileIdentity(verified[0].absolute, fileInfo), ParentIdentity: deliveryDirectoryObjectIdentity(parent)}}}
		if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
		_, release := svc.lockImportTarget(ctx, heldItem)
		done := make(chan error, 1)
		go func() { done <- svc.AbandonDeliveryDeleteRequest(ctx, r.ID, id) }()
		if sameTitle {
			select {
			case err := <-done:
				release()
				t.Fatalf("cancellation crossed its own title publication: %v", err)
			case <-time.After(40 * time.Millisecond):
			}
			release()
		}
		select {
		case err := <-done:
			if err != nil {
				release()
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			release()
			t.Fatal("cancellation waited behind another title or leaked its own authority")
		}
		release()
		saved, err := db.ExplicitDeliveryDeleteBatch(ctx, id)
		if err != nil || saved.AbandonedAt.IsZero() {
			t.Fatalf("request not durably cancelled: %+v %v", saved, err)
		}
		if bytes, err := os.ReadFile(verified[0].absolute); err != nil || string(bytes) != "video" {
			t.Fatalf("cancellation changed payload bytes: %q %v", bytes, err)
		}
	}
}

// Admission may unlock before network IO; deferred cleanup must remain safe
// after that early release, and a panic must release both authority layers.
func TestStorageDecisionReleaseIsIdempotentAndPanicSafe(t *testing.T) {
	svc := &Service{}
	ctx, release := svc.lockStorageDecision(context.Background())
	_, releaseNested := svc.lockStorageDecision(ctx)
	releaseNested()
	release()
	release()
	// Reusing an expired context must acquire a fresh lease.
	ctx, release = svc.lockStorageDecision(ctx)
	releaseNested() // an old nested release cannot release this lease
	release()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fixture did not panic")
			}
		}()
		targetCtx, releaseTarget := svc.lockImportTarget(ctx, 7)
		defer releaseTarget()
		_, releaseDecision := svc.lockStorageDecision(targetCtx)
		defer releaseDecision()
		panic("admission interrupted")
	}()
	done := make(chan struct{})
	go func() {
		targetCtx, releaseTarget := svc.lockImportTarget(context.Background(), 7)
		defer releaseTarget()
		_, releaseDecision := svc.lockStorageDecision(targetCtx)
		defer releaseDecision()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panic leaked admission authority")
	}
}
