package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// Removing a file by hand was the one library operation with no test at all,
// and it is the one that deletes bytes. Everything below asserts the contract
// the doc comment on RemoveFile states: the row goes first, the bytes second,
// deleting and blocklisting are separate statements, and whatever did not work
// is reported rather than swallowed.

// addFile puts a real file on disk, records it against the item, and returns
// its id — the shape RemoveFile expects to be handed.
func acq2AddFile(t *testing.T, svc *Service, itemID, copyID int64, dir, name, release string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := svc.db.UpsertFile(ctx, itemID, copyID, path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if release != "" {
		if err := svc.db.SetFileSource(ctx, id, release, "idx"); err != nil {
			t.Fatal(err)
		}
	}
	return id, path
}

// The whole point of the feature: one call removes the row, removes the bytes,
// bans the release that produced them, and starts hunting a replacement. If any
// one of those silently does not happen the user is left believing it did.
func TestAcq2RemoveFileDeletesBlocklistsAndSearches(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t,
		[]ports.Release{rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 20)}, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)

	const bad = "Test.Movie.2024.1080p.WEB-DL.x264-BAD"
	fileID, path := acq2AddFile(t, svc, movieID, 0, item.Path, "Test Movie (2024).mkv", bad)

	res, err := svc.RemoveFile(ctx, movieID, fileID, RemoveFileRequest{
		FromDisk: true, Blocklist: true, Search: true, Reason: "audio is out of sync",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DeletedFromDisk {
		t.Errorf("result says the bytes survived: %+v", res)
	}
	if res.Blocklisted != bad {
		t.Errorf("blocklisted = %q, want %q", res.Blocklisted, bad)
	}
	if !res.Searched {
		t.Errorf("no replacement search was started: %+v", res)
	}
	if res.Note != "" {
		t.Errorf("nothing went wrong, so there should be no note: %q", res.Note)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the file is still on disk")
	}
	files, _ := db.ListFilesForItem(ctx, movieID)
	if len(files) != 0 {
		t.Errorf("the row survived the removal: %+v", files)
	}
	// The ban has to outlive the call, or automation grabs the same bad rip
	// back within the hour.
	if blocked, err := db.IsBlocklisted(ctx, bad, "idx"); err != nil || !blocked {
		t.Errorf("blocklisted = %v err %v", blocked, err)
	}
	// "Where did my file go" is asked days later, by which time the log has
	// rotated — so it has to be in history.
	events, _ := db.ListHistory(ctx)
	var sawRemoval bool
	for _, e := range events {
		if e.Type == HistoryFileRemoved && e.ReleaseTitle == bad {
			sawRemoval = true
		}
	}
	if !sawRemoval {
		t.Errorf("no %s history entry: %+v", HistoryFileRemoved, events)
	}
	// The search is a real search: it went to the indexer and grabbed.
	if len(client.added) != 1 {
		t.Errorf("replacement grabs = %v, want exactly one", client.added)
	}
}

// A file monarr adopted off disk has no source release, and inventing one from
// the filename would blocklist a string no indexer ever offered. The deletion
// still happens — the note is the difference between "we did not" and "we
// could not".
func TestAcq2RemoveFileWithoutASourceCannotBlocklist(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)
	fileID, path := acq2AddFile(t, svc, movieID, 0, item.Path, "adopted.mkv", "")

	res, err := svc.RemoveFile(ctx, movieID, fileID, RemoveFileRequest{
		FromDisk: true, Blocklist: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocklisted != "" {
		t.Errorf("blocklisted %q for a file monarr never grabbed", res.Blocklisted)
	}
	if !strings.Contains(res.Note, "no release to blocklist") {
		t.Errorf("note does not explain why: %q", res.Note)
	}
	if !res.DeletedFromDisk {
		t.Error("the deletion itself should still have happened")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file still on disk")
	}
}

// The file id comes off a URL. A mismatched (item, file) pair means somebody's
// stale tab is one click from deleting a stranger's file, so it is refused
// before anything is touched.
func TestAcq2RemoveFileRefusesAFileFromAnotherItem(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)
	fileID, path := acq2AddFile(t, svc, movieID, 0, item.Path, "mine.mkv", "")

	other, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Another", SortTitle: "another", Year: 2001,
		IDs: domain.ExternalIDs{TMDB: 777}, Monitored: true, Path: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RemoveFile(ctx, other, fileID, RemoveFileRequest{FromDisk: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("the file was deleted despite the mismatch")
	}
}

// A read-only mount or a permission problem must not read as success. The row
// is already gone by then (deliberately — the next scan re-adopts the file),
// so the only honest report is "removed from the library, but…".
func TestAcq2RemoveFileReportsAnUnlinkThatFailed(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)

	// A non-empty directory standing in for anything os.Remove refuses.
	stubborn := filepath.Join(item.Path, "stubborn")
	if err := os.MkdirAll(filepath.Join(stubborn, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, movieID, 0, stubborn, 1)
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.RemoveFile(ctx, movieID, fileID, RemoveFileRequest{FromDisk: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.DeletedFromDisk {
		t.Error("claimed to have deleted something it could not")
	}
	if !strings.Contains(res.Note, "could not be deleted") {
		t.Errorf("note = %q, want the failure spelled out", res.Note)
	}
}

// The bytes already being gone is exactly what was asked for, so it is not a
// failure — the row was the last thing left of the file and it is gone too.
func TestAcq2RemoveFileTreatsAnAlreadyMissingFileAsDone(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)
	fileID, path := acq2AddFile(t, svc, movieID, 0, item.Path, "ghost.mkv", "")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	res, err := svc.RemoveFile(ctx, movieID, fileID, RemoveFileRequest{FromDisk: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DeletedFromDisk || res.Note != "" {
		t.Errorf("a file that was already gone should read as done: %+v", res)
	}
}

// Search is best-effort and reports itself. With no indexers configured there
// is nothing to search, and saying "searched" would leave the user waiting for
// a replacement that is never coming.
func TestAcq2RemoveFileSaysWhenTheSearchCouldNotStart(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	indexers, _ := db.ListIndexers(ctx)
	for _, ix := range indexers {
		if err := db.DeleteIndexer(ctx, ix.ID); err != nil {
			t.Fatal(err)
		}
	}
	item, _ := db.GetMediaItemFull(ctx, movieID)
	fileID, _ := acq2AddFile(t, svc, movieID, 0, item.Path, "lonely.mkv", "")

	res, err := svc.RemoveFile(ctx, movieID, fileID, RemoveFileRequest{FromDisk: true, Search: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Searched {
		t.Error("claimed to have searched with no indexers configured")
	}
	if !strings.Contains(res.Note, "could not start a search") {
		t.Errorf("note = %q", res.Note)
	}
}

// A series has no item-wide wantable — "which season" is not a question a file
// deletion answers — so the replacement hunt falls back to the item sweep
// rather than giving up.
func TestAcq2RemoveFileFallsBackToTheItemSweepForASeries(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, []ports.Release{
		{Title: "Test.Show.S01.1080p.WEB-DL-PACK", DownloadURL: "u", Indexer: "idx",
			Protocol: "torrent", Seeders: 5, Size: 1000},
	}, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)
	fileID, _ := acq2AddFile(t, svc, itemID, 0,
		filepath.Join(item.Path, "Season 1"), "Test Show - S01E01 - Pilot [HDTV 720p].mkv", "")

	res, err := svc.RemoveFile(ctx, itemID, fileID, RemoveFileRequest{FromDisk: true, Search: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Searched {
		t.Errorf("a series deletion did not start a search: %+v", res)
	}
	if len(client.added) == 0 {
		t.Error("the fallback sweep never reached the indexer")
	}
}

// Deleting a copy's file must search for a replacement for THAT copy — the
// copy carries its own profile and its own folder, and the primary's state
// says nothing about what the copy is missing.
func TestAcq2RemoveFileSearchesForTheCopyItBelongedTo(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t,
		[]ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-NEW", 9)}, client)
	ctx := context.Background()

	copyDir := t.TempDir()
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: movieID, Name: "for dad", QualityProfileID: 1,
		Path: copyDir, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fileID, _ := acq2AddFile(t, svc, movieID, copyID, copyDir, "copy.mkv", "")

	res, err := svc.RemoveFile(ctx, movieID, fileID, RemoveFileRequest{FromDisk: true, Search: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Searched {
		t.Fatalf("no search for the copy: %+v", res)
	}
	active, _ := db.ListActiveDownloads(ctx)
	if len(active) != 1 {
		t.Fatalf("downloads = %+v", active)
	}
	if active[0].CopyID != copyID {
		t.Errorf("the replacement was grabbed for copy %d, want %d", active[0].CopyID, copyID)
	}
}

// Two things going wrong must both be reported. A single-slot note would have
// the blocklist failure quietly overwrite the unlink failure.
func TestAcq2NotesAccumulateRatherThanOverwrite(t *testing.T) {
	if got := joinNote("", "first"); got != "first" {
		t.Errorf("joinNote empty = %q", got)
	}
	if got := joinNote("first", "second"); got != "first; second" {
		t.Errorf("joinNote = %q", got)
	}
}
