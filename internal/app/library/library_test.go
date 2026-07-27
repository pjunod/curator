package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// fakeProvider implements ports.MetadataProvider with canned data.
type fakeProvider struct{}

func (fakeProvider) SearchMovies(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return []ports.SearchResult{{Kind: domain.KindMovie, TMDBID: 550, Title: "Fight Club", Year: 1999}}, nil
}

func (fakeProvider) SearchSeries(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return []ports.SearchResult{{Kind: domain.KindSeries, TMDBID: 100, Title: "Test Show", Year: 2020}}, nil
}

// movieRuntime is the runtime fakeProvider reports for a movie, in minutes.
//
// Zero by default, and that is load-bearing rather than lazy. Every media
// fixture in this package is a 64 KiB header window off a one-second clip, so
// any nonzero runtime makes all of them correctly implausible (a one-second
// file of a 139-minute feature is a sample) and every probe-wiring test starts
// failing for a reason that has nothing to do with wiring.
//
// Tests that want the duration rule to fire set it, and say so.
var movieRuntime = 0

func (fakeProvider) GetMovie(ctx context.Context, id int64) (domain.MediaItem, error) {
	return domain.MediaItem{
		Kind: domain.KindMovie, Title: "Fight Club", SortTitle: "fight club",
		Year: 1999, IDs: domain.ExternalIDs{TMDB: id}, Runtime: movieRuntime,
	}, nil
}

func (fakeProvider) GetSeries(ctx context.Context, id int64) (domain.MediaItem, error) {
	return domain.MediaItem{
		Kind: domain.KindSeries, Title: "Test Show", SortTitle: "test show",
		Year: 2020, IDs: domain.ExternalIDs{TMDB: id},
		Seasons: []domain.Season{{
			Number: 1, Monitored: true,
			Episodes: []domain.Episode{
				{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", Monitored: true},
				{SeasonNumber: 1, EpisodeNumber: 2, Title: "Two", Monitored: true},
			},
		}},
	}, nil
}

func newService(t *testing.T) (*Service, *sqlite.DB, *bus.Bus) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)
	return New(db, fakeProvider{}, b, nil), db, b
}

func TestAddMovieWithRootFolder(t *testing.T) {
	svc, db, b := newService(t)
	ctx := context.Background()
	events, cancel := bus.Subscribe[MediaAdded](b, 8)
	defer cancel()

	rootDir := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, rootDir, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}

	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(rootDir, "Fight Club (1999)")
	if item.Path != want {
		t.Errorf("path = %q, want %q", item.Path, want)
	}
	if item.RootFolderID != rf.ID || !item.Monitored {
		t.Errorf("item = %+v", item)
	}

	select {
	case e := <-events:
		if e.Title != "Fight Club" {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MediaAdded not published")
	}

	// Duplicate rejected.
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, Monitored: true}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate add err = %v", err)
	}
	_ = db
}

func TestAddSeriesCreatesTree(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Seasons) != 1 || len(item.Seasons[0].Episodes) != 2 {
		t.Fatalf("tree = %+v", item.Seasons)
	}
}

func TestSearchKindValidation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	// Books are live (Phase 2.5) but need a provider wired.
	if _, err := svc.Search(ctx, domain.KindBook, "dune"); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Errorf("book search without provider err = %v", err)
	}
	if _, err := svc.Search(ctx, "podcast", "x"); !errors.Is(err, ErrUnsupportedKind) {
		t.Errorf("unknown kind err = %v", err)
	}
	res, err := svc.Search(ctx, domain.KindMovie, "fight")
	if err != nil || len(res) != 1 {
		t.Errorf("movie search = %v, %v", res, err)
	}
}

func TestAddRootFolderValidation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.AddRootFolder(ctx, "relative/path", domain.KindMixed); err == nil {
		t.Error("relative path should be rejected")
	}
	if _, err := svc.AddRootFolder(ctx, "/definitely/not/a/real/dir-xyz", domain.KindMixed); err == nil {
		t.Error("missing dir should be rejected")
	}
	f := filepath.Join(t.TempDir(), "afile")
	os.WriteFile(f, []byte("x"), 0o644)
	if _, err := svc.AddRootFolder(ctx, f, domain.KindMixed); err == nil {
		t.Error("plain file should be rejected")
	}
}

func TestScanReconciles(t *testing.T) {
	svc, db, b := newService(t)
	ctx := context.Background()
	scans, cancel := bus.Subscribe[ScanCompleted](b, 8)
	defer cancel()

	root := t.TempDir()
	rf, _ := svc.AddRootFolder(ctx, root, domain.KindMixed)

	// The series folder matches the naming template "Test Show (2020)".
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, RootFolderID: rf.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	seasonDir := filepath.Join(item.Path, "Season 1")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	epFile := filepath.Join(seasonDir, "Test.Show.S01E01.1080p.mkv")
	os.WriteFile(epFile, []byte("fake video"), 0o644)
	os.WriteFile(filepath.Join(seasonDir, "notes.txt"), []byte("not a video"), 0o644)

	// An unclaimed directory in the root.
	os.MkdirAll(filepath.Join(root, "Some Random Show"), 0o755)

	// An item whose folder never appeared on disk.
	ghost, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}

	report, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.ItemsScanned != 1 || report.FilesLinked != 1 {
		t.Errorf("report = %+v", report)
	}
	if len(report.UnmatchedDirs) != 1 || report.UnmatchedDirs[0].Name != "Some Random Show" {
		t.Errorf("unmatched = %+v", report.UnmatchedDirs)
	}
	if len(report.MissingPaths) != 1 || report.MissingPaths[0] != ghost.Path {
		t.Errorf("missing = %+v", report.MissingPaths)
	}

	// The episode file is linked: S01E01 has a file.
	got, _ := svc.Get(ctx, item.ID)
	if len(got.Files) != 1 {
		t.Fatalf("files = %+v", got.Files)
	}
	if !got.Seasons[0].Episodes[0].HasFile || got.Seasons[0].Episodes[1].HasFile {
		t.Errorf("episode file flags wrong: %+v", got.Seasons[0].Episodes)
	}

	select {
	case e := <-scans:
		if e.FilesLinked != 1 || e.UnmatchedDirs != 1 {
			t.Errorf("event = %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ScanCompleted not published")
	}

	// Idempotent: second scan links nothing new.
	report2, _ := svc.Scan(ctx)
	if report2.FilesLinked != 0 || report2.FilesRemoved != 0 {
		t.Errorf("second scan should be a no-op: %+v", report2)
	}

	// Deleting the file on disk prunes the record on the next scan.
	os.Remove(epFile)
	report3, _ := svc.Scan(ctx)
	if report3.FilesRemoved != 1 {
		t.Errorf("third scan removed = %d, want 1", report3.FilesRemoved)
	}
	got, _ = svc.Get(ctx, item.ID)
	if len(got.Files) != 0 || got.Seasons[0].Episodes[0].HasFile {
		t.Errorf("file record should be pruned: %+v", got.Files)
	}

	// Report persisted and readable.
	persisted, ok, err := svc.LastScanReport(ctx)
	if err != nil || !ok || persisted.ScannedAt.IsZero() {
		t.Errorf("persisted report = %+v ok=%v err=%v", persisted, ok, err)
	}
	_ = db
}

// fakeBooks implements ports.BookProvider with canned data.
type fakeBooks struct{}

func (fakeBooks) SearchBooks(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return []ports.SearchResult{{
		Kind: domain.KindBook, OLID: "OL17091839W", Title: "Project Hail Mary",
		Author: "Andy Weir", Year: 2021,
	}}, nil
}

func (fakeBooks) GetBook(ctx context.Context, olid string) (domain.MediaItem, error) {
	return domain.MediaItem{
		Kind: domain.KindBook, Title: "Project Hail Mary", SortTitle: "project hail mary",
		Author: "Andy Weir", Year: 2021,
		IDs: domain.ExternalIDs{OLID: olid, ISBN13: "9780593135204"},
	}, nil
}

func TestAddBook(t *testing.T) {
	svc, db, _ := newService(t)
	svc.WithBooks(fakeBooks{})
	ctx := context.Background()

	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Search(ctx, domain.KindBook, "hail mary")
	if err != nil || len(got) != 1 || got[0].Author != "Andy Weir" {
		t.Fatalf("book search = %+v err %v", got, err)
	}

	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindBook, OLID: "OL17091839W", RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.Author != "Andy Weir" || item.IDs.OLID != "OL17091839W" || item.IDs.ISBN13 == "" {
		t.Errorf("book item = %+v", item)
	}
	// Calibre-friendly derived path + Ebook profile default.
	want := filepath.Join(root, "Andy Weir", "Project Hail Mary")
	if item.Path != want {
		t.Errorf("path = %q, want %q", item.Path, want)
	}
	if item.QualityProfileID != 4 {
		t.Errorf("profile = %d, want 4 (Ebook)", item.QualityProfileID)
	}

	// Same work twice → duplicate.
	if _, err := svc.Add(ctx, AddRequest{Kind: domain.KindBook, OLID: "OL17091839W"}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("dup add err = %v", err)
	}

	// Explicit Audiobook profile is honored.
	_ = db
}
