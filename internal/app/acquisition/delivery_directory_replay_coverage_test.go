package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryDirectoryRemovalStampFailureIsScheduledAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "imported" {
		t.Fatalf("receipt was not imported: %+v err=%v", r, err)
	}
	files, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("library placement=%+v err=%v", files, err)
	}
	libraryPath := files[0].Path

	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_directory_cleanup_stamp BEFORE UPDATE OF directory_cleanup_state ON runner_delivery_receipts WHEN NEW.directory_cleanup_state='removed' BEGIN SELECT RAISE(ABORT,'simulated directory cleanup stamp outage'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err == nil || !strings.Contains(err.Error(), "simulated directory cleanup stamp outage") {
		t.Fatalf("directory cleanup stamp trigger did not fail after removal: %v", err)
	}
	if _, err := os.Lstat(publication); !os.IsNotExist(err) {
		t.Fatalf("accepted empty publication tree was not removed before the injected database failure: %v", err)
	}
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("cleanup damaged the committed library file: %v", err)
	}
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT directory_cleanup_state FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "retry" {
		t.Fatalf("post-removal database failure left no scheduled retry: state=%q", state)
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER fail_directory_cleanup_stamp`); err != nil {
		t.Fatal(err)
	}
	// The retry is normally delayed by the cleanup backoff. Advance its durable
	// deadline to model a later scheduler sweep without sleeping in the test.
	if _, err := reopened.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET directory_cleanup_next_attempt_at=0 WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &deliveryImportClient{} })
	restarted.importCh = make(chan importJob, 1)
	restarted.importCtx = ctx
	restarted.importJobs = make(map[int64]*importTicket)
	restarted.deliveryCleanupJobs = make(map[int64]bool)
	restarted.scheduleDeliveryFileCleanup(ctx, cfg)
	select {
	case job := <-restarted.importCh:
		if !job.deliveryCleanupOnly || job.deliveryReceiptID != receiptID {
			t.Fatalf("scheduler queued the wrong durable cleanup obligation: %+v", job)
		}
		restarted.runImportTracked(job.ctx, job)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	default:
		t.Fatal("scheduler did not rediscover the directory cleanup retry after reopening SQLite")
	}
	if err := reopened.R.QueryRowContext(ctx, `SELECT directory_cleanup_state FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removed" {
		t.Fatalf("absent accepted directory did not settle the replayed obligation: state=%q", state)
	}
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("scheduled cleanup replay changed library media: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(publication)); err != nil {
		t.Fatalf("cleanup replay removed the storage root: %v", err)
	}
}

func TestDeliveryDirectoryRemovalEventFailureKeepsPrewrittenRetryAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_directory_event_finish BEFORE UPDATE OF finished_at ON runner_delivery_attempt_events WHEN NEW.result='succeeded' AND (SELECT stage FROM runner_delivery_attempt_events WHERE id=NEW.id)='cleanup_directory' BEGIN SELECT RAISE(ABORT,'simulated directory event outage'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err == nil || !strings.Contains(err.Error(), "simulated directory event outage") {
		t.Fatalf("directory event trigger did not fail after filesystem removal: %v", err)
	}
	if _, err := os.Lstat(publication); !os.IsNotExist(err) {
		t.Fatalf("directory event trigger fired before publication removal: %v", err)
	}
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT directory_cleanup_state FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("directory removal had no durable prewritten obligation after event failure: state=%q", state)
	}
	var running int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_attempt_events WHERE receipt_id=? AND stage='cleanup_directory' AND result='running'`, receiptID).Scan(&running); err != nil || running != 1 {
		t.Fatalf("fault did not leave one interrupted directory attempt: running=%d err=%v", running, err)
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER fail_directory_event_finish`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &deliveryImportClient{} })
	restarted.importCh = make(chan importJob, 1)
	restarted.importCtx = ctx
	restarted.importJobs = make(map[int64]*importTicket)
	restarted.deliveryCleanupJobs = make(map[int64]bool)
	restarted.scheduleDeliveryFileCleanup(ctx, cfg)
	select {
	case job := <-restarted.importCh:
		restarted.runImportTracked(job.ctx, job)
	default:
		t.Fatal("scheduler did not find the prewritten directory retry after reopening SQLite")
	}
	if err := reopened.R.QueryRowContext(ctx, `SELECT directory_cleanup_state FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removed" {
		t.Fatalf("replay did not finish directory cleanup: state=%q", state)
	}
}

func TestEmptyDeliveryDirectoryCleanupRejectsReplacedPublicationDirectory(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.PublicationDirectoryIdentity == "" {
		t.Fatalf("fixture has no accepted publication directory identity: receipt=%+v err=%v", r, err)
	}
	held := publication + ".held-for-directory-review"
	if err := os.Rename(publication, held); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(held) }()
	if err := os.Mkdir(publication, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(publication, "replacement-directory.txt")
	if err := os.WriteFile(sentinel, []byte("unrelated replacement generation"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := svc.removeEmptyDeliveryDirectories(ctx, r)
	if err == nil || removed {
		t.Fatalf("cleanup accepted an unreviewed same-path publication directory: removed=%v err=%v", removed, err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "unrelated replacement generation" {
		t.Fatalf("replacement directory contents were removed or changed: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(held, epRelease+".mkv")); err != nil {
		t.Fatalf("original accepted publication was not retained for review: %v", err)
	}
}

func TestDeliveryFileCleanupKeepsMovedPublicationPendingForReview(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.PublicationDirectoryIdentity == "" {
		t.Fatalf("fixture has no accepted publication directory identity: receipt=%+v err=%v", r, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
	if err != nil || row.CleanupState == "removed" {
		t.Fatalf("fixture cleanup obligation=%+v err=%v", row, err)
	}
	held := publication + ".externally-moved"
	if err := os.Rename(publication, held); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(held) }()
	// A cleanup sweep can finish its other work and report nil while persisting
	// this file as a retry. The authority check is that it did not claim the
	// bytes were removed after the accepted directory moved.
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatalf("cleanup sweep returned unexpectedly: %v", err)
	}
	row, err = db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
	if err != nil || row.CleanupState != "retry" {
		t.Fatalf("moved publication was recorded as deleted: row=%+v err=%v", row, err)
	}
	if got, err := os.ReadFile(filepath.Join(held, epRelease+".mkv")); err != nil || string(got) != "video" {
		t.Fatalf("moved accepted bytes were changed: %q err=%v", got, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_next_attempt_at=0 WHERE receipt_id=? AND path=?`, receiptID, epRelease+".mkv"); err != nil {
		t.Fatal(err)
	}
	reopened := reopenLifecycleDB(t, db)
	if err := reopened.R.QueryRowContext(ctx, `SELECT cleanup_state,cleanup_error FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, epRelease+".mkv").Scan(&row.CleanupState, &row.CleanupError); err != nil {
		t.Fatal(err)
	}
	if row.CleanupState != "retry" || !strings.Contains(row.CleanupError, "accepted publication directory changed") {
		t.Fatalf("moved publication did not retain a durable reviewable file obligation: row=%+v", row)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &deliveryImportClient{} })
	restarted.importCh = make(chan importJob, 1)
	restarted.importCtx = ctx
	restarted.importJobs = make(map[int64]*importTicket)
	restarted.deliveryCleanupJobs = make(map[int64]bool)
	restarted.scheduleDeliveryFileCleanup(ctx, cfg)
	select {
	case job := <-restarted.importCh:
		if !job.deliveryCleanupOnly || job.deliveryReceiptID != receiptID {
			t.Fatalf("moved publication retry scheduled wrong work: %+v", job)
		}
	default:
		t.Fatal("scheduler did not preserve the moved-publication cleanup review after restart")
	}
}

func TestDismissedImportedActivityStillSchedulesReceiptOwnedCleanup(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	files, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("library placement=%+v err=%v", files, err)
	}
	libraryPath := files[0].Path
	if dismissed, err := db.ClearImportedDownloads(ctx); err != nil || dismissed == 0 {
		t.Fatalf("dismiss imported Activity: rows=%d err=%v", dismissed, err)
	}
	retained, err := db.GetDownload(ctx, dl.ID)
	var dismissed int
	if queryErr := db.R.QueryRowContext(ctx, `SELECT activity_dismissed FROM downloads WHERE id=?`, dl.ID).Scan(&dismissed); err != nil || queryErr != nil || dismissed != 1 {
		t.Fatalf("pending receipt custody was not retained behind dismissed Activity: download=%+v dismissed=%d getErr=%v queryErr=%v", retained, dismissed, err, queryErr)
	}
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = make(map[int64]*importTicket)
	svc.deliveryCleanupJobs = make(map[int64]bool)
	svc.scheduleDeliveryFileCleanup(ctx, cfg)
	select {
	case job := <-svc.importCh:
		if !job.deliveryCleanupOnly || job.deliveryReceiptID != receiptID {
			t.Fatalf("dismissed receipt scheduled wrong work: %+v", job)
		}
		svc.runImportTracked(job.ctx, job)
	default:
		t.Fatal("dismissed Activity caused the durable cleanup receipt to disappear from due work")
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); !os.IsNotExist(err) {
		t.Fatalf("receipt-owned completed payload was not cleaned after Activity dismissal: %v", err)
	}
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("receipt cleanup removed the imported library file: %v", err)
	}
	if receipt, err := db.DeliveryReceiptByID(ctx, receiptID); err != nil || receipt.State != "imported" {
		t.Fatalf("durable lifecycle receipt disappeared after Activity dismissal: %+v err=%v", receipt, err)
	}
}
