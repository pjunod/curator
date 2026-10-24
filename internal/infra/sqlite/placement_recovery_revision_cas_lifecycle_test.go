package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestCommitPlacementRejectsStaleRecoveryRevisionWithoutPartialReceipt(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Recovery revision CAS", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := db.LibraryRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES('placement-revision-cas','importing',json_object('accepted_revision',?),?)`, revision, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	p := Placement{
		ID: "placement-revision-cas-file", ItemID: itemID, Target: "/library/revision-cas.mkv",
		Size: 42, SHA256: "accepted-recovery-digest", RecoveryImport: "placement-revision-cas",
		RecoveryFile: "recovery-file-1", State: "prepared",
	}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil || !strings.Contains(err.Error(), "library changed during recovery") {
		t.Fatalf("stale recovery placement commit error=%v, want revision conflict", err)
	}
	assertUncommitted := func() {
		t.Helper()
		var files, placements, results int
		var state string
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, p.Target).Scan(&files); err != nil {
			t.Fatal(err)
		}
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements WHERE placement_id=?`, p.ID).Scan(&placements); err != nil {
			t.Fatal(err)
		}
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id=? AND file_id=?`, p.RecoveryImport, p.RecoveryFile).Scan(&results); err != nil {
			t.Fatal(err)
		}
		if err := db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if files != 0 || placements != 0 || results != 0 || state != "prepared" {
			t.Fatalf("stale recovery revision left partial commit: files=%d placements=%d results=%d state=%q", files, placements, results, state)
		}
	}
	assertUncommitted()

	if _, err := db.W.ExecContext(ctx, `UPDATE recovery_imports SET data=json_set(data,'$.accepted_revision',(SELECT revision FROM lifecycle_library_revision WHERE id=1)) WHERE id=?`, p.RecoveryImport); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.CommitPlacement(ctx, p)
	if err != nil || fileID == 0 {
		t.Fatalf("prepared recovery intent did not commit after fresh revision acceptance: fileID=%d err=%v", fileID, err)
	}
	var files, placements, results int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=? AND path=? AND file_generation=?`, fileID, p.Target, p.ID).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements WHERE placement_id=? AND media_file_id=?`, p.ID, fileID).Scan(&placements); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id=? AND file_id=?`, p.RecoveryImport, p.RecoveryFile).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if files != 1 || placements != 1 || results != 1 {
		t.Fatalf("fresh recovery retry did not commit one exact result: files=%d placements=%d results=%d", files, placements, results)
	}
}
