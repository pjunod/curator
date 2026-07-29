package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// Books are the third kind, and they go down a different branch at every step:
// their own wantable, their own search plan, their own file layout, and — ADR
// 0006 — a quality that comes from the extension rather than from a probe.
// Until now none of that branch was exercised at the service level, so a change
// to the video path could quietly take the book path with it.

// bookSetup builds a monitored, missing book on the Ebook profile, with one
// enabled indexer and client.
func bookSetup(t *testing.T, releases []ports.Release, client *fakeClient) (*Service, *sqlite.DB, int64) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)
	svc := New(db, b, nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{releases: releases} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindBook, Title: "Project Hail Mary", SortTitle: "project hail mary",
		Author: "Andy Weir", Year: 2021, IDs: domain.ExternalIDs{OLID: "OL17091839W"},
		Monitored: true, Path: t.TempDir(), QualityProfileID: quality.EbookProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "idx", URL: "http://x",
		Protocol: "torrent", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "qbittorrent",
		Name: "qb", URL: "http://qb", Category: "monarr", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return svc, db, id
}

// The book path end to end: wanted → searched → grabbed → placed as
// "Title - Author.ext" in the item folder (Calibre-friendly, ADR 0006).
func TestAcq2BookIsWantedSearchedAndImported(t *testing.T) {
	client := &fakeClient{}
	release := "Andy Weir - Project Hail Mary (2021) EPUB"
	svc, db, bookID := bookSetup(t, []ports.Release{{
		Title: release, DownloadURL: "http://dl/book", Indexer: "idx",
		Protocol: "torrent", Seeders: 3, Size: 2 << 20,
	}}, client)
	ctx := context.Background()

	// 1. Wanted, and described the way a person names a book.
	wanted, err := svc.WantedList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 1 || !wanted[0].Missing {
		t.Fatalf("wanted = %+v", wanted)
	}
	if wanted[0].Title != "Project Hail Mary" || wanted[0].Detail != "by Andy Weir" {
		t.Errorf("wanted entry = %+v, want the author as the detail", wanted[0])
	}

	// 2. The automatic search finds and grabs it.
	if err := svc.AutoSearchItem(ctx, bookID); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatalf("book grabs = %v, want one", client.added)
	}
	active, _ := db.ListActiveDownloads(ctx)
	if len(active) != 1 || active[0].WantableIDs[0] != "book:"+itoa(bookID) {
		t.Fatalf("download = %+v, want a book wantable id", active)
	}

	// 3. The import places it under the Calibre-friendly name, and grades it
	//    from the extension rather than from a probe.
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "phm.epub"), []byte("epub bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := active[0]
	res, err := svc.importDownload(ctx, dl, payload, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d: %+v", res.Imported, res.Files)
	}
	files, _ := db.ListFilesForItem(ctx, bookID)
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	if base := filepath.Base(files[0].Path); !strings.Contains(base, "Project Hail Mary") ||
		!strings.Contains(base, "Andy Weir") || !strings.HasSuffix(base, ".epub") {
		t.Errorf("book filename = %q, want \"Title - Author.epub\"", base)
	}
	recs, err := db.FileQualityRecords(ctx, bookID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v err %v", recs, err)
	}
	if recs[0].Quality.Source != quality.SourceEPUB {
		t.Errorf("recorded source = %v, want epub — the extension is the authority", recs[0].Quality.Source)
	}
}

// A better format replaces the worse one; a worse format is refused outright.
// This is the profile doing its job on the book axis, where "better" is a
// format ladder rather than a resolution.
func TestAcq2BookUpgradeReplacesAndDowngradeIsRefused(t *testing.T) {
	svc, db, bookID := bookSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, bookID)

	// A PDF already on the shelf.
	pdf := filepath.Join(item.Path, "Project Hail Mary - Andy Weir.pdf")
	if err := os.WriteFile(pdf, []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, bookID, 0, pdf, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Recorded exactly the way the book importer records one: the extension is
	// a claim somebody made about this specific file, so it is verified — which
	// is what lets a better format legitimately replace it.
	if err := db.SetFileQualityFrom(ctx, fid, quality.Quality{Source: quality.SourcePDF},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}

	// An EPUB beats it: imported, and the PDF is taken away.
	better := t.TempDir()
	if err := os.WriteFile(filepath.Join(better, "phm.epub"), []byte("epub"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: bookID, ReleaseTitle: "Andy Weir - Project Hail Mary EPUB", Indexer: "idx",
	}, better, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 1 || !res.Upgraded {
		t.Fatalf("epub over pdf should be an upgrade: %+v", res)
	}
	if _, err := os.Stat(pdf); !os.IsNotExist(err) {
		t.Error("the replaced pdf is still on disk")
	}

	// A MOBI does not: automation is gated by the profile, which is what a
	// profile is for.
	worse := t.TempDir()
	if err := os.WriteFile(filepath.Join(worse, "phm.mobi"), []byte("mobi"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = svc.importDownload(ctx, sqlite.Download{
		MediaItemID: bookID, ReleaseTitle: "Andy Weir - Project Hail Mary MOBI", Indexer: "idx",
	}, worse, false)
	if err == nil {
		t.Fatalf("a downgrade was imported: %+v", res)
	}
	if !strings.Contains(err.Error(), "does not improve on") {
		t.Errorf("err = %v, want it to say why", err)
	}
	files, _ := db.ListFilesForItem(ctx, bookID)
	if len(files) != 1 || !strings.HasSuffix(files[0].Path, ".epub") {
		t.Errorf("shelf after the refused downgrade = %+v", files)
	}
}

// The user pointed at the file and pressed Import. The profile gates
// AUTOMATION; second-guessing a person who has already decided is the same
// mistake as gating a manual grab.
func TestAcq2ManualBookImportIsNotGatedByTheProfile(t *testing.T) {
	svc, db, bookID := bookSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, bookID)

	epub := filepath.Join(item.Path, "Project Hail Mary - Andy Weir.epub")
	if err := os.WriteFile(epub, []byte("epub"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, _ := db.UpsertFile(ctx, bookID, 0, epub, 4)
	if err := db.SetFileQualityFrom(ctx, fid, quality.Quality{Source: quality.SourceEPUB},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}

	worse := t.TempDir()
	if err := os.WriteFile(filepath.Join(worse, "phm.mobi"), []byte("mobi"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ManualImport(ctx, ManualImportRequest{Path: worse, MediaItemID: bookID})
	if err != nil {
		t.Fatalf("a manual book import was refused: %v", err)
	}
	if res.Imported != 1 {
		t.Fatalf("imported = %d: %+v", res.Imported, res.Files)
	}
	// …but it must not DELETE the better copy on its way in.
	if _, err := os.Stat(epub); err != nil {
		t.Error("the manual import of a worse format deleted the better one")
	}
}

// describeTarget is what a rejected candidate says it did not match. Getting it
// wrong turns "does not match Project Hail Mary by Andy Weir" into "does not
// match target", which is the same as saying nothing.
func TestAcq2DescribeTargetNamesEveryKind(t *testing.T) {
	cases := []struct {
		w    domain.Wantable
		want string
	}{
		{domain.MovieWantable{Title: "Heat", Year: 1995}, "Heat (1995)"},
		{domain.EpisodeWantable{Title: "Test Show", Season: 1, Episode: 2}, "Test Show S01E02"},
		{domain.SeasonWantable{Title: "Test Show", Season: 3}, "Test Show season 3"},
		{domain.BookWantable{Title: "Project Hail Mary", Author: "Andy Weir"}, "Project Hail Mary by Andy Weir"},
		{domain.BookWantable{Title: "Beowulf"}, "Beowulf"},
	}
	for _, c := range cases {
		if got := describeTarget(c.w); got != c.want {
			t.Errorf("describeTarget(%T) = %q, want %q", c.w, got, c.want)
		}
	}
}
