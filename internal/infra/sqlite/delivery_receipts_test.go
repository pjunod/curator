package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryReceiptFeedUsesStableBoundedPages(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		v := ports.RunnerDelivery{Sequence: uint64(i), DeliveryID: fmt.Sprintf("page-%d", i), PublicationGeneration: "g", ArtifactID: "a", JobID: "17", RequestToken: "t", RunnerInstanceID: "runner", PublicationPath: "/completed/a", ManifestDigest: "digest", ReadyAt: time.Unix(1, 0), Files: []ports.RunnerFile{{Path: "x.mkv", SizeBytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
		seedLegacyReceipt(t, db, 1, v, "/mounted/a")
	}
	first, next, err := db.DeliveryReceiptPage(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || next != first[1].ID {
		t.Fatalf("first page=%d next=%d", len(first), next)
	}
	second, next, err := db.DeliveryReceiptPage(ctx, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || next != 0 || second[0].ID >= first[1].ID {
		t.Fatalf("second page=%+v next=%d", second, next)
	}
	filtered, next, err := db.DeliveryReceiptPageForDownload(ctx, 999, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 || next != 0 {
		t.Fatalf("download-filtered page=%+v next=%d", filtered, next)
	}
}

// A row whose publication still has files on disk is hidden by Clear, never
// deleted (the publication cascades with it); it goes once the publication's
// per-file cleanup removes the directory (contract section 3.4).
func TestClearImportedDismissesRowWithLivePublicationAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(91,'movie','Delivery','delivery',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key,payload_removed,import_path) VALUES(91,91,'Delivery',3,'usenet','17','t-17','imported',1,1,'generation-91',0,'/completed/delivery')`); err != nil {
		t.Fatal(err)
	}
	pubID, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: 91, Generation: "g1", Kind: "completed", Path: "/completed/delivery"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetPublicationState(ctx, pubID, "imported", ""); err != nil {
		t.Fatal(err)
	}
	hidden, err := db.ClearImportedDownloads(ctx)
	if err != nil || hidden != 1 {
		t.Fatalf("ClearImportedDownloads hidden=%d err=%v", hidden, err)
	}
	var n int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE id=91 AND activity_dismissed=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a row with a live publication was not kept hidden: count=%d err=%v", n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PruneTerminalDownloads(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM publications WHERE id=?`, pubID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("retention deleted a live publication with its row: count=%d err=%v", n, err)
	}
	if err := db.MarkPublicationRemoved(ctx, pubID); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE id=91`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the hidden row outlived its removed publication: count=%d err=%v", n, err)
	}
}

func TestClearFailedSoftDismissesRowWithLiveSalvage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(92,'movie','Failed delivery','failed delivery',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key) VALUES(92,92,'Failed delivery',3,'usenet','18','t-18','failed',1,1,'generation-92')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: 92, Generation: "g2", Kind: "salvage", Path: "/completed/failed"}); err != nil {
		t.Fatal(err)
	}
	cleared, err := db.ClearFailedDownloads(ctx)
	if err != nil || cleared != 1 {
		t.Fatalf("ClearFailedDownloads cleared=%d err=%v", cleared, err)
	}
	var count, dismissed int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*),max(activity_dismissed) FROM downloads WHERE id=92`).Scan(&count, &dismissed); err != nil || count != 1 || dismissed != 1 {
		t.Fatalf("a failed row with a live salvage was not kept hidden: count=%d dismissed=%d err=%v", count, dismissed, err)
	}
}
