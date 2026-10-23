package acquisition

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestBadReplacementUnlinkBeforeDatabaseCommitReconcilesAfterRestart(t *testing.T) {
	svc, db, ctx, op, reviewed, source := reviewedBadReplacementFixture(t)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_replacement_record_delete BEFORE UPDATE OF removal_state ON replacement_operation_files WHEN NEW.removal_state='removed' BEGIN SELECT RAISE(ABORT,'simulated replacement receipt outage'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.removeBadReplacementFile(ctx, op, reviewed); err == nil || !strings.Contains(err.Error(), "simulated replacement receipt outage") {
		t.Fatalf("replacement file record trigger did not fail after authorized unlink: %v", err)
	}
	quarantine := filepath.Join(filepath.Dir(source), replacementQuarantinePrefix+op.ID+"-"+strconv.FormatInt(reviewed.MediaFileID, 10))
	for _, path := range []string{source, quarantine} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("injected database failure occurred before exact reviewed bytes were retired at %q: %v", path, err)
		}
	}
	files, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(files) != 1 || files[0].RemovalState != "pending" {
		t.Fatalf("database failure did not preserve the durable removal obligation: files=%+v err=%v", files, err)
	}
	if _, err := db.GetMediaFileCustodyByPath(ctx, source); err != nil {
		t.Fatalf("record was deleted despite the transaction fault: %v", err)
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER fail_replacement_record_delete`); err != nil {
		t.Fatal(err)
	}
	svc.db = reopened
	svc.processReplacementOperations(ctx)
	if _, err := reopened.GetMediaFileCustodyByPath(ctx, source); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("worker did not reconcile exact absent source into the library database: %v", err)
	}
	files, err = reopened.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(files) != 1 || files[0].RemovalState != "removed" {
		t.Fatalf("replayed removal obligation did not settle its durable audit row: files=%+v err=%v", files, err)
	}
	updated, err := reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil || (updated.State != "searching" && updated.State != "waiting_for_release") {
		t.Fatalf("worker did not advance after exact source cleanup: operation=%+v err=%v", updated, err)
	}
}
