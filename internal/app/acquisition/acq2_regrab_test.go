package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The re-grab loop, from monarr's side (docs: nzbd REGRAB_LOOP_PLAN M1–M4).
//
// The field shape: a complete season pack on disk, half its episodes in the
// library, the other half still listed as missing, no error anywhere, the
// payload cleaned up, and automation grabbing the same pack again the next
// night. Every assertion here is one station of that.

// M1: an import that stops on a full disk is a FAILED import, however many
// files it placed first — and the payload is not cleaned up out from under
// the half that never landed.
func TestImportStoppedByAFullDiskFailsVisibly(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	payload := t.TempDir()
	for _, name := range []string{
		"Test.Show.S01E01.1080p.WEB-DL.mkv",
		"Test.Show.S01E02.1080p.WEB-DL.mkv",
	} {
		if err := os.WriteFile(filepath.Join(payload, name), corpusFile(t, wholeCorpus720), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dl := insertImportable(t, db, itemID, payload)

	// Every placement answers ENOSPC — the error the field produced.
	restore := failPlacement(t, syscall.ENOSPC)
	defer restore()

	err := svc.runImport(ctx, dl)
	if err == nil {
		t.Fatal("a half-imported payload reported success — this is the defect")
	}
	if !errors.Is(err, ErrImportBlocked) {
		t.Fatalf("import failed for the wrong reason: %v", err)
	}
	row, gerr := db.GetDownload(ctx, dl.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if row.State != "failed" {
		t.Errorf("state %q, want failed — a stopped import must be visible", row.State)
	}
	if !blockedImport(row) {
		t.Errorf("the row is not recognised as retryable: state=%q err=%q", row.State, row.Error)
	}
	if len(client.removed) != 0 {
		t.Error("cleaned up the payload after an import that did not finish")
	}
	if _, err := os.Stat(payload); err != nil {
		t.Errorf("the payload directory is gone: %v", err)
	}
}

// M1, second half: once there is room, the sweep finishes the job. Nobody
// has to notice; that is the point.
func TestBlockedImportIsRetriedWhenSpaceReturns(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	payload := t.TempDir()
	src := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(src, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, payload)

	restore := failPlacement(t, syscall.ENOSPC)
	if err := svc.runImport(ctx, dl); err == nil {
		t.Fatal("expected the import to fail on a full disk")
	}

	// Space returns. The sweep's backoff exists so a volume someone is
	// clearing gets time to be cleared; a test does not have fifteen
	// minutes.
	restore()
	prevBackoff := importRetryBackoff
	importRetryBackoff = 0
	defer func() { importRetryBackoff = prevBackoff }()
	if err := svc.RetryBlockedImports(ctx); err != nil {
		t.Fatalf("retry sweep: %v", err)
	}
	row, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "imported" {
		t.Fatalf("state %q, want imported — the retry did not finish the job (err=%q)",
			row.State, row.Error)
	}
	if importAttempts(row) != 1 {
		t.Errorf("attempts recorded: %d, want 1", importAttempts(row))
	}
}

// Environmental failures remain retryable; the delay increases but never
// reaches a permanent attempt cap.
func TestImportRetriesBackOffWithoutPermanentCap(t *testing.T) {
	dl := sqlite.Download{State: "failed", Error: ErrImportBlocked.Error() + ": disk", ImportPath: "/x"}
	if !blockedImport(dl) {
		t.Fatal("a blocked import must be retryable")
	}
	for i := 0; i < 20; i++ {
		dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{Step: stepImportRetry})
	}
	if importAttempts(dl) != 20 || !blockedImport(dl) {
		t.Fatalf("blocked import stopped after %d attempts", importAttempts(dl))
	}
	if got := importRetryDelay(0); got != 30*time.Second {
		t.Fatalf("first retry delay=%s", got)
	}
	if got := importRetryDelay(1); got != time.Minute {
		t.Fatalf("second retry delay=%s", got)
	}
	if got := importRetryDelay(2); got != 2*time.Minute {
		t.Fatalf("third retry delay=%s", got)
	}
	if got := importRetryDelay(3); got != 5*time.Minute {
		t.Fatalf("fourth retry delay=%s", got)
	}
	if got := importRetryDelay(20); got != 5*time.Minute {
		t.Fatalf("steady retry delay=%s", got)
	}
	dl.DeliveryExpected = true
	if blockedImport(dl) {
		t.Error("delivery-managed imports must use the durable manifest retry journal")
	}
	// And an import failure that is NOT environmental is never retried.
	other := sqlite.Download{State: "failed", Error: "no files imported from /x — bad names", ImportPath: "/x"}
	if blockedImport(other) {
		t.Error("a payload monarr cannot parse is not a disk problem; retrying it is a loop")
	}
}

func TestAutomaticImportRetryLeavesLegacyReviewCohortUntouched(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()
	payload := t.TempDir()
	src := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(src, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, payload)
	restore := failPlacement(t, syscall.ENOSPC)
	if err := svc.runImport(ctx, dl); err == nil {
		t.Fatal("expected blocked import")
	}
	restore()
	if _, err := db.R.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, payload); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	prev := importRetryBackoff
	importRetryBackoff = 0
	defer func() { importRetryBackoff = prev }()
	if err := svc.RetryBlockedImports(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != before.State || len(after.Handoff) != len(before.Handoff) {
		t.Fatalf("retry changed legacy row before review: state %q->%q, handoff %d->%d", before.State, after.State, len(before.Handoff), len(after.Handoff))
	}
}

// Native Runner owns cleanup authority; a failed response leaves its payload
// and Curator retry record intact instead of bypassing Runner through the mount.
func TestRunnerPayloadRemainsPendingWhenClientCannotConfirmDeletion(t *testing.T) {
	client := &fakeClient{removeErr: errors.New("history entry not found")}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	payload := filepath.Join(t.TempDir(), "Test.Show.S01.COMPLETE")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(src, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, payload)

	if err := svc.runImport(ctx, dl); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Errorf("unconfirmed Runner payload must remain: %v", err)
	}
	rows, err := db.ImportedWithPayload(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("expected pending Runner cleanup row, got %d", len(rows))
	}
}

// M2, the guard: a path mapping that points at the library is not a licence
// to delete the library.
func TestPayloadDirInsideARootFolderIsNeverRemoved(t *testing.T) {
	client := &fakeClient{removeErr: errors.New("nope")}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	roots, err := db.ListRootFolders(ctx)
	if err != nil || len(roots) == 0 {
		t.Skipf("no root folder in the fixture (%v)", err)
	}
	inside := filepath.Join(roots[0].Path, "Some.Release")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, filepath.Join(t.TempDir(), "reported-source"))
	svc.removeImportedDir(ctx, downloadRef{
		ID: dl.ID, MediaItemID: itemID, ClientID: dl.ClientID,
		GenerationKey: dl.GenerationKey, ImportPath: inside, ReleaseTitle: "x",
	})
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("a directory inside a root folder was removed: %v", err)
	}
}

// M3: the same release is never grabbed twice while the first one is still
// moving. Jobs 238/239 on nuc3 were the same NZB six minutes apart.
func TestGrabRefusesADuplicateInFlight(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	req := GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1, Protocol: "usenet",
		Title: "Test.Show.S01E01.1080p.WEB-DL-GRP", Indexer: "idx",
		DownloadURL: "http://x/1.nzb", Size: 1 << 30,
	}
	first, err := svc.Grab(ctx, req)
	if err != nil {
		t.Fatalf("first grab: %v", err)
	}
	// Same release, different separators — an indexer's punctuation is not
	// a new release.
	dupe := req
	dupe.Title = "Test Show S01E01 1080p WEB-DL GRP"
	second, err := svc.Grab(ctx, dupe)
	if err != nil {
		t.Fatalf("second grab: %v", err)
	}
	if second != first {
		t.Errorf("grabbed it twice: %d then %d", first, second)
	}
	if len(client.added) != 1 {
		t.Errorf("the client was asked to download it %d times, want 1", len(client.added))
	}
	rows, err := db.ListActiveDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("%d active rows for one release", len(rows))
	}
}

// M3, the bound: a want whose grabs keep failing stops burning wire.
func TestRegrabIsCappedPerWindow(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < reGrabLimit; i++ {
		id, err := db.InsertDownload(ctx, sqlite.Download{
			MediaItemID: itemID, WantableIDs: []string{"episode:1:1:1"},
			ReleaseTitle: "Burned.Release." + strings.Repeat("x", i+1),
			Protocol:     "usenet", ClientID: clients[0].ID, State: "grabbed",
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		row.State = "failed"
		if err := db.UpdateDownloadHandoff(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	w, err := svc.wantableFromID(ctx, "episode:1:1:1")
	if err != nil {
		t.Skipf("fixture has no such wantable: %v", err)
	}
	capped, spent := svc.regrabCapped(ctx, w)
	if !capped {
		t.Errorf("not capped after %d failed grabs in the window", spent)
	}
}

// M4: the timeline monarr always recorded and never showed.
func TestHistoryIsReadable(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	ctx := context.Background()

	if err := db.AddHistory(ctx, "grabbed", itemID, "Rel", map[string]any{"indexer": "idx"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddHistory(ctx, HistoryImportBlocked, itemID, "Rel",
		map[string]any{"placed": 5, "of": 10}); err != nil {
		t.Fatal(err)
	}
	events, err := svc.History(ctx)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Type)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "grabbed") || !strings.Contains(joined, HistoryImportBlocked) {
		t.Errorf("the timeline is missing its own events: %s", joined)
	}
}

// failPlacement makes every file placement fail with errno until the
// returned function is called. A full volume cannot be arranged in a unit
// test, and running as root makes a chmod fixture succeed anyway.
func failPlacement(t *testing.T, errno syscall.Errno) func() {
	t.Helper()
	prev := placeFile
	placeFile = func(_ context.Context, src, dest string, onBytes func(done, total int64)) error {
		return &os.PathError{Op: "write", Path: dest, Err: errno}
	}
	restored := false
	restore := func() {
		if !restored {
			placeFile, restored = prev, true
		}
	}
	t.Cleanup(restore)
	return restore
}

var _ = ports.ProtocolOfClient
