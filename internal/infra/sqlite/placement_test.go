package sqlite

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
)

func TestPlacementReceiptAndMetadataAreAtomic(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Recovered", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES('import','importing',json_object('accepted_revision',(SELECT revision FROM lifecycle_library_revision WHERE id=1)),0)`); err != nil {
		t.Fatal(err)
	}
	p := Placement{ID: "placement", ItemID: item, Target: "/library/recovered.mkv", Size: 42, SHA256: "digest", RecoveryImport: "import", RecoveryFile: "file", State: "prepared", Quality: quality.Quality{Resolution: 1080}}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	// A failed episode FK must roll back the file, placement and receipt together.
	p.EpisodeIDs = []int64{999999}
	if _, err = db.CommitPlacement(ctx, p); err == nil {
		t.Fatal("invalid episode unexpectedly committed")
	}
	var files, receipts int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if files != 0 || receipts != 0 {
		t.Fatalf("partial transaction: files=%d receipts=%d", files, receipts)
	}
	p.EpisodeIDs = nil
	fid, err := db.CommitPlacement(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if fid == 0 {
		t.Fatal("missing file id")
	}
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id='import' AND file_id='file'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("receipts=%d", receipts)
	}
	// Replaying an old placement cannot overwrite a later metadata correction.
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_release='corrected' WHERE id=?`, fid); err != nil {
		t.Fatal(err)
	}
	if _, err = db.CommitPlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	var release string
	if err = db.R.QueryRowContext(ctx, `SELECT source_release FROM media_files WHERE id=?`, fid).Scan(&release); err != nil {
		t.Fatal(err)
	}
	if release != "corrected" {
		t.Fatalf("replay overwrote correction: %s", release)
	}
}
