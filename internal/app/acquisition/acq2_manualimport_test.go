package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// ScanImportPath is the direct answer to "why didn't my download import": if it
// returns nothing, that is the answer. So what it does and does not list is the
// behaviour, not an implementation detail.
func TestAcq2ScanImportPathListsMediaAndSkipsSamples(t *testing.T) {
	svc, _, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()

	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("Test.Show.S01E02.1080p.WEB-DL.mkv")
	write("sample.mkv") // a sample is not the release
	write("readme.nfo") // not media at all
	bookPath := write("Some.Book.epub")

	got, err := svc.ScanImportPath(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ScannedFile{}
	for _, f := range got {
		byName[f.Name] = f
	}
	if len(got) != 2 {
		t.Fatalf("scanned %d files, want the episode and the book only: %+v", len(got), got)
	}
	ep, ok := byName["Test.Show.S01E02.1080p.WEB-DL.mkv"]
	if !ok {
		t.Fatalf("the episode was not listed: %+v", got)
	}
	if ep.Kind != "video" || ep.Season != 1 || len(ep.Episodes) != 1 || ep.Episodes[0] != 2 {
		t.Errorf("episode guess = %+v, want S01E02 video", ep)
	}
	if ep.Quality == "" || ep.Size == 0 {
		t.Errorf("scan did not report quality/size: %+v", ep)
	}
	book, ok := byName["Some.Book.epub"]
	if !ok {
		t.Fatalf("the book was not listed: %+v", got)
	}
	if book.Kind != "book" || book.Quality != "epub" {
		t.Errorf("book guess = %+v, want an epub book", book)
	}
	if book.Path != bookPath {
		t.Errorf("book path = %q", book.Path)
	}
}

func TestManualImportDefaultsToTheCompletedFolderMonarrSees(t *testing.T) {
	svc, db, _ := setup(t, nil, &fakeClient{})
	clients, err := db.ListDownloadClients(context.Background())
	if err != nil || len(clients) == 0 {
		t.Fatalf("clients = %+v, err = %v", clients, err)
	}
	clients[0].PathMappings = []ports.PathMapping{{Remote: "/downloads/done", Local: "/working/monarr/completed"}}
	if err := db.UpdateDownloadClient(context.Background(), clients[0]); err != nil {
		t.Fatal(err)
	}
	if got := svc.ManualImportDefaultPath(context.Background()); got != "/working/monarr/completed" {
		t.Fatalf("manual import default = %q", got)
	}
}

// Pointing at one file is as valid as pointing at a folder — and pointing at a
// file that is not media has to come back empty rather than pretending.
func TestAcq2ScanImportPathAcceptsASingleFile(t *testing.T) {
	svc, _, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	dir := t.TempDir()

	video := filepath.Join(dir, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(video, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ScanImportPath(ctx, video)
	if err != nil || len(got) != 1 || got[0].Path != video {
		t.Fatalf("single-file scan = %+v err %v", got, err)
	}

	other := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = svc.ScanImportPath(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a text file was offered as importable media: %+v", got)
	}
}

// An empty or unreachable path is the common case behind a failed manual
// import, and the message is the whole value: "path not accessible" tells the
// user to look at their mounts, silence tells them nothing.
func TestAcq2ScanImportPathRejectsPathsItCannotUse(t *testing.T) {
	svc, _, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()

	if _, err := svc.ScanImportPath(ctx, "   "); err == nil {
		t.Error("an empty path was accepted")
	}
	_, err := svc.ScanImportPath(ctx, filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(err.Error(), "not accessible") {
		t.Errorf("err = %v, want the path named as inaccessible", err)
	}
}

// The escape hatch when automation cannot resolve a payload: the user points at
// a folder, and the stuck queue row has to end up saying what happened. A
// manual import that leaves the row stuck is half a feature.
func TestAcq2ManualImportMarksTheStuckRowImported(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-STUCK", DownloadURL: "m",
		Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL-STUCK.mkv"),
		[]byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := svc.ManualImport(ctx, ManualImportRequest{
		Path: payload, MediaItemID: itemID, DownloadID: id,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d: %+v", res.Imported, res.Files)
	}
	row, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "imported" {
		t.Fatalf("row state = %q after a successful manual import", row.State)
	}
	if row.ImportPath != payload {
		t.Errorf("row import path = %q, want %q", row.ImportPath, payload)
	}
	var saidManual bool
	for _, h := range row.Handoff {
		if h.Step == stepImported && contains(h.Detail, "manually imported") {
			saidManual = true
		}
	}
	if !saidManual {
		t.Errorf("the trace does not record that a human did this: %+v", row.Handoff)
	}
}

// When the manual import fails the row must say so too, or the user retries
// against a row that still claims to be waiting.
func TestAcq2ManualImportMarksTheRowFailedWhenNothingLands(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-EMPTY", DownloadURL: "m",
		Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir() // no media in it at all

	if _, err := svc.ManualImport(ctx, ManualImportRequest{
		Path: empty, MediaItemID: itemID, DownloadID: id,
	}); err == nil {
		t.Fatal("importing an empty folder reported success")
	}
	row, _ := db.GetDownload(ctx, id)
	if row.State != "failed" {
		t.Fatalf("row state = %q, want failed", row.State)
	}
	if row.Error == "" {
		t.Error("the row carries no reason for the failure")
	}
}

// Without a target there is nothing to import INTO, and guessing one from a
// folder name is how a stranger's film lands in the wrong library entry.
func TestAcq2ManualImportRefusesWithoutATarget(t *testing.T) {
	svc, _, _ := setup(t, nil, &fakeClient{})
	if _, err := svc.ManualImport(context.Background(), ManualImportRequest{
		Path: t.TempDir(),
	}); err == nil {
		t.Fatal("a manual import with no item chosen was accepted")
	}
}

// The preview is a chooser, not decoration. When two files are listed and
// the user selects one, the other one must not be imported merely because it
// shares the scanned directory.
func TestAcq2ManualImportImportsOnlySelectedFiles(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	payload := t.TempDir()
	chosen := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	other := filepath.Join(payload, "Test.Show.S01E02.1080p.WEB-DL.mkv")
	for _, p := range []string{chosen, other} {
		if err := os.WriteFile(p, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := svc.ManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{chosen}, MediaItemID: itemID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 || len(res.Files) != 1 || res.Files[0].Name != filepath.Base(chosen) {
		t.Fatalf("selected import = %+v, want only %s", res, filepath.Base(chosen))
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || strings.Contains(files[0].Path, "E02") {
		t.Fatalf("library files = %+v, unselected file was imported", files)
	}
}

func TestAcq2ManualImportRejectsASelectionOutsideTheScannedPath(t *testing.T) {
	svc, _, itemID := setup(t, nil, &fakeClient{})
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "Test.Show.S01E01.mkv")
	if err := os.WriteFile(outside, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := svc.ManualImport(context.Background(), ManualImportRequest{
		Path: root, Paths: []string{outside}, MediaItemID: itemID,
	})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside selection err = %v", err)
	}
}

func TestAcq2ManualImportRejectsAnEmptyExplicitSelection(t *testing.T) {
	svc, _, itemID := setup(t, nil, &fakeClient{})
	_, err := svc.ManualImport(context.Background(), ManualImportRequest{
		Path: t.TempDir(), Paths: []string{}, MediaItemID: itemID,
	})
	if err == nil || !strings.Contains(err.Error(), "no files selected") {
		t.Fatalf("empty selection err = %v", err)
	}
}

func TestAcq2ManualImportFileSelectionCannotSwitchToASibling(t *testing.T) {
	svc, _, itemID := setup(t, nil, &fakeClient{})
	dir := t.TempDir()
	root := filepath.Join(dir, "Test.Show.S01E01.mkv")
	sibling := filepath.Join(dir, "Test.Show.S01E02.mkv")
	for _, p := range []string{root, sibling} {
		if err := os.WriteFile(p, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.ManualImport(context.Background(), ManualImportRequest{
		Path: root, Paths: []string{sibling}, MediaItemID: itemID,
	})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("sibling selection err = %v", err)
	}
}

// A download id that does not exist is a stale tab, not an import — it must
// fail before anything is placed.
func TestAcq2ManualImportRejectsAnUnknownDownload(t *testing.T) {
	svc, _, itemID := setup(t, nil, &fakeClient{})
	if _, err := svc.ManualImport(context.Background(), ManualImportRequest{
		Path: t.TempDir(), MediaItemID: itemID, DownloadID: 99999,
	}); err == nil {
		t.Fatal("an unknown download id was accepted")
	}
}

// A manual import can be pointed at a copy, and it has to land in the COPY's
// folder — the whole reason a copy exists is that it is kept somewhere else.
func TestAcq2ManualImportHonoursTheChosenCopy(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	copyDir := t.TempDir()
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "for dad", QualityProfileID: 1,
		Path: copyDir, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.720p.HDTV.mkv"),
		[]byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.ManualImport(ctx, ManualImportRequest{
		Path: payload, MediaItemID: itemID, CopyID: copyID,
	}); err != nil {
		t.Fatal(err)
	}
	files, _ := db.ListFilesForItem(ctx, itemID)
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	if files[0].CopyID != copyID || !strings.HasPrefix(files[0].Path, copyDir) {
		t.Errorf("manual import landed outside the copy: %+v", files[0])
	}
}

func TestQueuedManualImportReturnsBeforeCopyAndPersistsTheSelection(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	svc.SetRegistry(transfers.New())
	workerCtx, stop := context.WithCancel(context.Background())
	svc.StartImporters(workerCtx)
	t.Cleanup(func() { stop(); svc.WaitImporters() })

	payload := t.TempDir()
	selected := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL-MANUAL.mkv")
	if err := os.WriteFile(selected, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	previous := placeFile
	placeFile = func(ctx context.Context, _, _ string, _ func(done, total int64)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() { placeFile = previous })

	id, err := svc.QueueManualImport(context.Background(), ManualImportRequest{
		Path: payload, Paths: []string{selected}, MediaItemID: itemID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("queued manual import returned no Activity row")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued manual import did not reach a worker")
	}
	dl, err := db.GetDownload(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if dl.Protocol != "manual" || dl.MediaItemID != itemID {
		t.Fatalf("manual Activity row = %+v", dl)
	}
	found := false
	for _, h := range dl.Handoff {
		if h.Step == stepManualQueued && len(h.Paths) == 1 && h.Paths[0] == selected {
			found = true
		}
	}
	if !found {
		t.Fatalf("manual selection was not durable: %+v", dl.Handoff)
	}
	if svc.ImportStatus(id) == "" {
		t.Fatal("manual import is copying but Activity cannot see its importer")
	}
	if err := svc.CancelImport(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

var _ = sqlite.Download{}
var _ ports.Release
