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

// Field data 2026-11-12: a failed Runner row whose salvage was still pending
// was deleted by "Clear failed"; Runner's publication then arrived for a
// transfer Curator had no row for, and nothing ever imported or cleaned it.
// Clear failed and Remove hide such a row; once its salvage publication
// exists it follows the ordinary rule.
func TestClearFailedKeepsRowWhoseSalvageIsPending(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(93,'movie','M','m',1,1,1)`)
	for _, r := range []struct {
		id      int
		salvage string
	}{{93, "pending"}, {94, "none"}, {95, "pending"}} {
		exec(`INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key,runner_salvage) VALUES(?,93,'M',3,'usenet',?,?,'failed',1,1,?,?)`,
			r.id, r.id, "t-"+string(rune('a'+r.id-93)), "g-"+string(rune('a'+r.id-93)), r.salvage)
	}
	if _, err := db.ClearFailedDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	present := func(id int) (bool, bool) {
		var n, dismissed int
		if err := db.R.QueryRowContext(ctx, `SELECT count(*), COALESCE(max(activity_dismissed),0) FROM downloads WHERE id=?`, id).Scan(&n, &dismissed); err != nil {
			t.Fatal(err)
		}
		return n == 1, dismissed == 1
	}
	if ok, hidden := present(93); !ok || !hidden {
		t.Fatalf("pending-salvage row: present=%v hidden=%v, want kept hidden", ok, hidden)
	}
	if ok, _ := present(94); ok {
		t.Fatal("a failed row with no salvage coming was not cleared")
	}
	if err := db.DeleteDownload(ctx, 95); err != nil {
		t.Fatal(err)
	}
	if ok, hidden := present(95); !ok || !hidden {
		t.Fatalf("removed pending-salvage row: present=%v hidden=%v, want kept hidden", ok, hidden)
	}
	// The salvage arrived and its publication is gone: the row is ordinary.
	pubID, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: 93, Generation: "s", Kind: "salvage", Path: "/completed/s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPublicationRemoved(ctx, pubID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClearFailedDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := present(93); ok {
		t.Fatal("a row whose salvage publication came and went was kept")
	}
}

// Retention honours a pending salvage too: the row ages out only once Runner
// settled it.
func TestRetentionKeepsRowWhoseSalvageIsPending(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(96,'movie','M','m',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key,runner_salvage) VALUES(96,96,'M',3,'usenet','96','t-96','failed',1,1,'g-96','pending')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PruneTerminalDownloads(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE id=96`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("retention deleted a pending-salvage row: count=%d err=%v", n, err)
	}
	if err := db.SettleRunnerSalvage(ctx, 96, "none"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PruneTerminalDownloads(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE id=96`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a settled row outlived retention: count=%d err=%v", n, err)
	}
}

// A removed publication's path is never attached to again by a different
// payload: a new publication there replaces the removed record; a replay of
// the removed publication itself still finds it (it is not resurrected).
func TestInsertPublicationNeverAttachesToARemovedRow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(97,'movie','M','m',1,1,1)`)
	exec(`INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key) VALUES(97,97,'M',3,'usenet','97','t-97','imported',1,1,'g-97')`)
	exec(`INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key) VALUES(98,97,'M',3,'usenet','98','t-98','downloaded',1,1,'g-98')`)
	old, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: 97, Generation: "a", Kind: "completed", Path: "/completed/same"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPublicationRemoved(ctx, old); err != nil {
		t.Fatal(err)
	}
	if again, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: 97, Generation: "a", Kind: "completed", Path: "/completed/same"}); err != nil || again != old {
		t.Fatalf("replay of the removed publication = %d (%v), want %d", again, err, old)
	}
	fresh, err := db.InsertPublication(ctx, PublicationWrite{DownloadID: 98, Generation: "b", Kind: "completed", Path: "/completed/same"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := db.GetPublication(ctx, fresh) // SQLite may reuse the freed rowid
	if err != nil || p.State != "discovered" || p.DownloadID != 98 || p.Generation != "b" {
		t.Fatalf("new publication = %+v (%v)", p, err)
	}
}
