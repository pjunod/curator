package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestCommitPlacementRollsBackWhenRecoveryRevisionAdvanceFails(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Revision advance rollback", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := db.LibraryRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const importID = "revision-advance-failure"
	if _, err := db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES(?,?,json_object('accepted_revision',?),?)`, importID, "importing", revision, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	p := Placement{
		ID: "revision-advance-placement", ItemID: itemID, Target: "/library/revision-advance.mkv",
		Size: 73, SHA256: "verified-recovery-payload", RecoveryImport: importID,
		RecoveryFile: "manifest-file-1", State: "prepared",
	}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_recovery_revision_advance BEFORE UPDATE OF data ON recovery_imports
		WHEN NEW.id='revision-advance-failure' BEGIN SELECT RAISE(ABORT,'injected recovery revision advance failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil {
		t.Fatal("placement committed although its recovery revision advance failed")
	}
	assertRolledBack := func() {
		t.Helper()
		var files, placements, results int
		var state string
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, p.Target).Scan(&files); err != nil {
			t.Fatal(err)
		}
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements WHERE placement_id=?`, p.ID).Scan(&placements); err != nil {
			t.Fatal(err)
		}
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id=? AND file_id=?`, importID, p.RecoveryFile).Scan(&results); err != nil {
			t.Fatal(err)
		}
		if err := db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if files != 0 || placements != 0 || results != 0 || state != "prepared" {
			t.Fatalf("revision failure left partial placement authority: files=%d placements=%d results=%d state=%q", files, placements, results, state)
		}
	}
	assertRolledBack()
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_recovery_revision_advance`); err != nil {
		t.Fatal(err)
	}
	fid, err := db.CommitPlacement(ctx, p)
	if err != nil || fid == 0 {
		t.Fatalf("exact prepared placement did not retry after revision storage recovered: fileID=%d err=%v", fid, err)
	}
	var files, placements, results int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=? AND path=? AND file_generation=?`, fid, p.Target, p.ID).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements WHERE placement_id=? AND media_file_id=?`, p.ID, fid).Scan(&placements); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id=? AND file_id=?`, importID, p.RecoveryFile).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if files != 1 || placements != 1 || results != 1 {
		t.Fatalf("exact retry did not commit one atomic recovery placement: files=%d placements=%d results=%d", files, placements, results)
	}
	replayID, err := db.CommitPlacement(ctx, p)
	if err != nil || replayID != fid {
		t.Fatalf("committed placement replay changed file identity: fileID=%d want=%d err=%v", replayID, fid, err)
	}
}
