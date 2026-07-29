package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
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

var _ = sqlite.Download{}
var _ ports.Release
