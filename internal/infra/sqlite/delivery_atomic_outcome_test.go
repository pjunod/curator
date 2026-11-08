package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func atomicDeliveryFixture(t *testing.T) (*DB, DeliveryReceipt, Placement) {
	t.Helper()
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	t.Cleanup(func() { _ = db.Close() })
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Atomic ledger", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: item, State: "downloaded", ReleaseTitle: "Atomic ledger", Protocol: "usenet", Transfer: "atomic-transfer"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	v := ports.RunnerDelivery{Sequence: 1, DeliveryID: "atomic-delivery", PublicationGeneration: "atomic-publication", ArtifactID: "artifact", JobID: "7", RequestToken: dl.Transfer, RunnerInstanceID: "runner", PublicationPath: root, ManifestDigest: "digest", ReadyAt: time.Now(), Files: []ports.RunnerFile{{Path: "movie.mkv", SizeBytes: 42, SHA256: "digest"}}}
	r := seedLegacyReceipt(t, db, 1, v, root)
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET download_id=?,generation_key=?,media_item_id=? WHERE id=?`, dl.ID, dl.GenerationKey, item, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedLegacyFileOutcomes(t, db, r.ID, []DeliveryManifestFile{{Path: "movie.mkv", SizeBytes: 42, SHA256: "digest", FileGeneration: "source-stat"}}, nil)
	p := Placement{ID: "atomic-placement", SourceGeneration: dl.GenerationKey, SourceTransfer: dl.Transfer, SourcePayloadGeneration: v.PublicationGeneration, SourceFileGeneration: "source-stat", Source: filepath.Join(root, "movie.mkv"), Target: filepath.Join(t.TempDir(), "movie.mkv"), ItemID: item, Size: 42, SHA256: "digest", PublishedFileGeneration: "library-stat", State: "prepared"}
	return db, r, p
}
func TestPlacementAndDeliveryLedgerRollbackAtEveryWriteBoundary(t *testing.T) {
	for _, fault := range []string{"disposition", "cleanup", "provenance"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			db, r, p := atomicDeliveryFixture(t)
			if err := db.PreparePlacement(ctx, p); err != nil {
				t.Fatal(err)
			}
			trigger := ""
			switch fault {
			case "disposition":
				trigger = `CREATE TRIGGER fail_atomic BEFORE UPDATE OF disposition ON runner_delivery_file_outcomes BEGIN SELECT RAISE(ABORT,'disposition failure'); END`
			case "cleanup":
				trigger = `CREATE TRIGGER fail_atomic BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes BEGIN SELECT RAISE(ABORT,'cleanup failure'); END`
			case "provenance":
				trigger = `CREATE TRIGGER fail_atomic BEFORE INSERT ON runner_delivery_file_imports BEGIN SELECT RAISE(ABORT,'provenance failure'); END`
			}
			if _, err := db.W.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := db.CommitPlacement(ctx, p); err == nil {
				t.Fatal("placement survived ledger failure")
			}
			row, err := db.DeliveryFileOutcomeByPath(ctx, r.ID, "movie.mkv")
			if err != nil || row.Disposition != "pending" || row.CleanupState != "protected" {
				t.Fatalf("partial outcome=%+v err=%v", row, err)
			}
			var files int
			if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, p.Target).Scan(&files); err != nil || files != 0 {
				t.Fatalf("library metadata committed despite ledger failure: %d %v", files, err)
			}
			if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_atomic`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.CommitPlacement(ctx, p); err != nil {
				t.Fatal(err)
			}
			row, err = db.DeliveryFileOutcomeByPath(ctx, r.ID, "movie.mkv")
			imports, importErr := db.DeliveryFileImports(ctx, r.ID)
			if err != nil || importErr != nil || row.Disposition != "placed" || row.CleanupState != "pending" || len(imports) != 1 || imports[0].LibraryFileGeneration != p.ID {
				t.Fatalf("retry=%+v imports=%+v err=%v/%v", row, imports, err, importErr)
			}
		})
	}
}
