package acquisition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The copy fallback is the path every real deployment takes: fast local disk
// for downloads, big array for the library. A hardlink cannot cross that, so
// the bytes are copied — and until it was instrumented a 20 GB copy was
// invisible everywhere in the process.
func TestAcq2CrossFilesystemImportCopiesAndReportsItsBytes(t *testing.T) {
	other, err := os.MkdirTemp("/dev/shm", "monarr-xdev-*")
	if err != nil {
		t.Skip("no second filesystem available to copy across")
	}
	t.Cleanup(func() { _ = os.RemoveAll(other) })

	payload := bytes.Repeat([]byte("monarr"), 5000)
	src := filepath.Join(other, "src.mkv")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	// Confirm the two really are different filesystems, or this test is
	// asserting the hardlink path under a misleading name.
	probe := filepath.Join(destDir, "probe")
	if err := os.Link(src, probe); err == nil {
		_ = os.Remove(probe)
		t.Skip("temp dir and /dev/shm are the same filesystem")
	}

	var lastDone, lastTotal int64
	dest := filepath.Join(destDir, "Season 1", "out.mkv")
	if err := place(src, dest, func(done, total int64) { lastDone, lastTotal = done, total }); err != nil {
		t.Fatalf("cross-filesystem place: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("copied %d bytes, want %d", len(got), len(payload))
	}
	if lastDone != int64(len(payload)) || lastTotal != int64(len(payload)) {
		t.Errorf("progress reported %d/%d, want %d bytes copied", lastDone, lastTotal, len(payload))
	}
	// The copy path uses CreateTemp, which makes files 0600 — a mode no media
	// server can open. The explicit chmod is the whole reason this is here.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != DefaultFileMode {
		t.Errorf("copied file mode = %v, want %v", info.Mode().Perm(), DefaultFileMode)
	}
	// Nothing left over in the destination folder but the file itself.
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %+v", entries)
	}
}

// Every way place can fail has to fail loudly rather than report a file that
// is not there — a silent failure here is a library row pointing at nothing.
func TestAcq2PlaceReportsWhatItCouldNotDo(t *testing.T) {
	dir := t.TempDir()

	// A destination whose parent cannot be created, because a file is in the way.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src.mkv")
	if err := os.WriteFile(src, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := place(src, filepath.Join(blocker, "sub", "out.mkv"), nil); err == nil {
		t.Error("placed a file into a folder that cannot exist")
	}

	// A source that is not there at all.
	if err := place(filepath.Join(dir, "ghost.mkv"), filepath.Join(dir, "out.mkv"), nil); err == nil {
		t.Error("placed a source that does not exist")
	}

	// A source that cannot be read as a stream of bytes.
	srcDir := filepath.Join(dir, "adir")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := place(srcDir, filepath.Join(dir, "out2.mkv"), nil); err == nil {
		t.Error("copied a directory as if it were a file")
	}

	// A destination that cannot be renamed over.
	occupied := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(occupied, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := place(src, occupied, nil); err == nil {
		t.Error("renamed over a non-empty directory")
	}
}

// sizeOf answers for a file that is not there, because it is called on paths
// that may have moved — a panic or a wrong number there would corrupt the size
// recorded against a library row.
func TestAcq2SizeOfAMissingFileIsZero(t *testing.T) {
	if got := sizeOf(filepath.Join(t.TempDir(), "nothing")); got != 0 {
		t.Errorf("sizeOf(missing) = %d", got)
	}
}

// collectFiles is the front door of every import. Pointing it at a file, at a
// non-media file, and at a path that is not there are all real situations, and
// only one of them is an error.
func TestAcq2CollectFilesHandlesFilesFoldersAndNothing(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Test.Show.S01E01.mkv")
	if err := os.WriteFile(video, []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := collectFiles(video, isVideoForTest)
	if err != nil || len(got) != 1 || got[0] != video {
		t.Errorf("single video = %v err %v", got, err)
	}
	got, err = collectFiles(text, isVideoForTest)
	if err != nil || len(got) != 0 {
		t.Errorf("single non-media file = %v err %v", got, err)
	}
	if _, err := collectFiles(filepath.Join(dir, "gone"), isVideoForTest); err == nil {
		t.Error("a payload that is not there was accepted")
	}
}

func isVideoForTest(p string) bool { return strings.HasSuffix(p, ".mkv") }

// "No files imported" alone told a user only that something went wrong
// somewhere. The per-file reasons are the message they actually read — capped,
// with the remainder counted out loud, because silent truncation reads as
// "that was all of them".
func TestAcq2ImportReasonsAreCappedAndCountTheRemainder(t *testing.T) {
	var r ImportResult
	for i := 0; i < 5; i++ {
		r.Files = append(r.Files, FileOutcome{Name: "f" + itoa(int64(i)), Reason: "nope"})
	}
	r.Files = append(r.Files, FileOutcome{Name: "good", Imported: true})

	if got := r.reasons(2); got != "f0: nope; f1: nope; and 3 more" {
		t.Errorf("reasons(2) = %q", got)
	}
	if got := (ImportResult{}).reasons(3); got != "" {
		t.Errorf("reasons with nothing skipped = %q", got)
	}
	if n := len(r.Skipped()); n != 5 {
		t.Errorf("skipped = %d", n)
	}
}

// Each of these stops the import before a byte is moved, and each is a
// different thing for the user to go and fix.
func TestAcq2ImportRefusesWhatItCannotResolve(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.importDownload(ctx, sqlite.Download{MediaItemID: 424242}, payload, false); err == nil {
		t.Error("imported into an item that does not exist")
	}
	if _, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: itemID, CopyID: 999,
	}, payload, false); err == nil {
		t.Error("imported into a copy that has been deleted")
	}

	// An item with no library folder has nowhere for the file to go.
	homeless, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Homeless", SortTitle: "homeless", Year: 2020,
		IDs: domain.ExternalIDs{TMDB: 5555}, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.importDownload(ctx, sqlite.Download{MediaItemID: homeless}, payload, false)
	if err == nil || !strings.Contains(err.Error(), "library folder") {
		t.Errorf("err = %v, want the missing folder named", err)
	}

	// A payload with no media in it.
	if _, err := svc.importDownload(ctx, sqlite.Download{MediaItemID: itemID},
		t.TempDir(), false); err == nil {
		t.Error("imported an empty folder")
	}

	// A profile that has been deleted out from under the item.
	gone := int64(4242)
	if err := db.BulkUpdateItem(ctx, itemID, nil, &gone); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.importDownload(ctx, sqlite.Download{MediaItemID: itemID}, payload, false); err == nil {
		t.Error("imported under a profile that does not exist")
	}
}

// A payload monarr cannot place has to fail rather than half-land: a movie
// whose library folder is a regular file is a misconfiguration, and reporting
// success would leave a row pointing at a file that was never written.
func TestAcq2ImportFailsWhenTheLibraryFolderIsNotAFolder(t *testing.T) {
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	notADir := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	movieID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Blocked", SortTitle: "blocked", Year: 2019,
		IDs: domain.ExternalIDs{TMDB: 6666}, Monitored: true, Path: notADir,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Blocked.2019.1080p.WEB-DL.mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: movieID, ReleaseTitle: "Blocked.2019.1080p.WEB-DL-GRP",
	}, payload, false); err == nil {
		t.Error("an unplaceable movie file reported a successful import")
	}
}

// A single file often carries its quality only on the release NAME — the file
// inside a usenet post is routinely called something anonymous. Falling back to
// the grabbed quality is what stops that landing as "Unknown".
func TestAcq2AnAnonymousFileInheritsTheGrabbedQuality(t *testing.T) {
	svc, _, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL-GRP", Indexer: "idx",
		Quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
	}, payload, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d: %+v", res.Imported, res.Files)
	}
	if !strings.Contains(res.Files[0].Path, "WEB-DL 1080p") {
		t.Errorf("placed as %q — the release's quality was not carried over", res.Files[0].Path)
	}
}

// The episode importer has to work out which episodes a file covers, and say so
// when it cannot: a file naming episodes the show does not have, and a file
// naming no episode at all, are different refusals.
func TestAcq2EpisodeImportSaysWhenItCannotPlaceAFile(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()

	payload := t.TempDir()
	// A season the show does not have.
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S05E01.1080p.mkv"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Nothing episode-shaped about it at all.
	if err := os.WriteFile(filepath.Join(payload, "featurette.mkv"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Test.Show.S05.1080p-GRP", Indexer: "idx",
	}, payload, false)
	if err == nil {
		t.Fatal("placed files it could not map to any episode")
	}
	if !strings.Contains(err.Error(), "no known episodes") {
		t.Errorf("err = %v, want the unknown season named", err)
	}
	if !strings.Contains(err.Error(), "cannot determine episodes") {
		t.Errorf("err = %v, want the unparseable file named", err)
	}
	files, _ := db.ListFilesForItem(ctx, itemID)
	if len(files) != 0 {
		t.Errorf("rows were written for files that never landed: %+v", files)
	}
}

// A double episode is one file covering two slots, and its name has to say so
// — "S01E01" on a file that is also episode 2 loses the second episode to the
// next backlog pass forever.
func TestAcq2ADoubleEpisodeFileIsNamedForBothEpisodes(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01E02.1080p.WEB-DL.mkv"),
		[]byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E01E02.1080p.WEB-DL-GRP", Indexer: "idx",
	}, payload, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d: %+v", res.Imported, res.Files)
	}
	if !strings.Contains(res.Files[0].Path, "S01E01-E02") {
		t.Errorf("placed as %q, want both episodes in the name", res.Files[0].Path)
	}
	files, _ := db.ListFilesForItem(ctx, itemID)
	if len(files) != 1 || len(files[0].EpisodeIDs) != 2 {
		t.Errorf("episode links = %+v, want the file covering both", files)
	}
}

// An upgrade replaces only the files covering the episodes it actually
// supersedes. Deleting a sibling episode's file because a neighbour was
// upgraded is data loss with a progress bar.
func TestAcq2AnEpisodeUpgradeLeavesItsSiblingsAlone(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)
	seasonDir := filepath.Join(item.Path, "Season 1")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatal(err)
	}

	put := func(name string, season, episode int, q quality.Quality) string {
		p := filepath.Join(seasonDir, name)
		if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		fid, err := db.UpsertFile(ctx, itemID, 0, p, 3)
		if err != nil {
			t.Fatal(err)
		}
		epID, err := db.GetEpisodeID(ctx, itemID, season, episode)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ReplaceFileEpisodeLinks(ctx, fid, []int64{epID}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetFileQualityFrom(ctx, fid, q,
			mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old1 := put("Test Show - S01E01 - Pilot [HDTV 720p].mkv", 1, 1,
		quality.Quality{Source: quality.SourceHDTV, Resolution: 720})
	keep2 := put("Test Show - S01E02 - Finale [HDTV 720p].mkv", 1, 2,
		quality.Quality{Source: quality.SourceHDTV, Resolution: 720})

	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv"),
		[]byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL-GRP", Indexer: "idx",
	}, payload, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded {
		t.Errorf("1080p over 720p was not treated as an upgrade: %+v", res)
	}
	if _, err := os.Stat(old1); !os.IsNotExist(err) {
		t.Error("the superseded episode 1 file survived")
	}
	if _, err := os.Stat(keep2); err != nil {
		t.Error("upgrading episode 1 deleted episode 2's file")
	}
}

// A replaced file that cannot be unlinked (a read-only mount, a permission)
// must not take the import down with it: the new file is already in place and
// the row is already correct.
func TestAcq2AReplacedFileThatWillNotDeleteDoesNotFailTheImport(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)

	// A non-empty directory standing in for anything os.Remove refuses.
	stubborn := filepath.Join(item.Path, "Test Movie (2024) [HDTV-720p].mkv")
	if err := os.MkdirAll(filepath.Join(stubborn, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, movieID, 0, stubborn, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fid,
		quality.Quality{Source: quality.SourceHDTV, Resolution: 720},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}

	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Movie.2024.1080p.WEB-DL.mkv"),
		[]byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: movieID, ReleaseTitle: "Test.Movie.2024.1080p.WEB-DL-GRP", Indexer: "idx",
	}, payload, false)
	if err != nil {
		t.Fatalf("an undeletable old file failed the whole import: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d", res.Imported)
	}
	files, _ := db.ListFilesForItem(ctx, movieID)
	if len(files) != 1 || !strings.Contains(files[0].Path, "WEB-DL 1080p") {
		t.Errorf("library after the upgrade = %+v", files)
	}
	// The row is gone even though the bytes could not be; the next scan is
	// what re-adopts whatever is actually still there.
	if _, err := os.Stat(stubborn); err != nil {
		t.Errorf("the undeletable path vanished after all: %v", err)
	}
}

// A payload nobody grabbed has no source release to remember, and inventing one
// from the folder name would put a string no indexer ever offered onto a file —
// which is what "blocklist this release" would later act on.
func TestAcq2AnUngrabbedImportRemembersNoSourceRelease(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv"),
		[]byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.importDownload(ctx, sqlite.Download{MediaItemID: itemID}, payload, false); err != nil {
		t.Fatal(err)
	}
	files, _ := db.ListFilesForItem(ctx, itemID)
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	if release, _ := db.FileSource(ctx, files[0].ID); release != "" {
		t.Errorf("source release = %q for a payload nobody grabbed", release)
	}
}

// A payload that arrives near the advertised size is normal — par2 and rar
// overhead are real — and must not produce a short-delivery entry. The entry
// exists to point at the download client, and one that cries wolf is worse
// than none.
func TestAcq2AFullSizePayloadIsNotReportedAsShort(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	payload := t.TempDir()
	body := bytes.Repeat([]byte("x"), 1000)
	if err := os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv"),
		body, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL-GRP",
		Indexer: "idx", Size: 1100,
	}, payload, false); err != nil {
		t.Fatal(err)
	}
	events, _ := db.ListHistory(ctx)
	for _, e := range events {
		if e.Type == HistoryShortDelivery {
			t.Errorf("a payload at 91%% of the advertised size was reported short: %+v", e)
		}
	}
}

var _ ports.Release
