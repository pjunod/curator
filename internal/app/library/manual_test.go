package library

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

// manualRoot registers a root and returns it with a folder inside.
func manualRoot(t *testing.T, svc *Service, kind domain.RootKind, folder string, files ...string) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		full := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, kind); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The point of the feature: a folder no provider can place is still media,
// and dismissing it left it unmanaged.
func TestAddManualCreatesARecordWithNoProvider(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	dir := manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Christmas 1998")

	item, err := svc.AddManual(context.Background(), ManualRequest{
		Kind: domain.KindMovie, Title: "Christmas 1998", Year: 1998, Path: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !item.IsManual() {
		t.Errorf("source = %q, want manual", item.Source)
	}
	if item.Path != dir {
		t.Errorf("path = %q, want %q", item.Path, dir)
	}
	if item.IDs.TMDB != 0 || item.IDs.TVDB != 0 || item.IDs.OLID != "" {
		t.Errorf("a manual entry invents no ids, got %+v", item.IDs)
	}
	// Created unmonitored: title matching with no provider's alternate names
	// is weak, and the failure mode is grabbing something unrelated.
	if item.Monitored {
		t.Error("a manual entry starts unmonitored")
	}
}

// The episode list comes off the disk, because for a record with no provider
// the files ARE the metadata.
func TestManualSeriesReadsItsEpisodesFromTheFiles(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	dir := manualRoot(t, svc, domain.RootKindOf(domain.KindSeries), "Church Recordings",
		"Season 1/Church.Recordings.S01E01.mkv",
		"Season 1/Church.Recordings.S01E02.mkv",
		"Season 2/Church.Recordings.S02E01.mkv",
		// No episode number: skipped rather than guessed at. Inventing
		// S01E03 for it would collide with a correctly-named file later.
		"Season 1/some old tape.mkv",
		// Not video at all.
		"Season 1/notes.txt",
	)

	item, err := svc.AddManual(context.Background(), ManualRequest{
		Kind: domain.KindSeries, Title: "Church Recordings", Path: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Seasons) != 2 {
		t.Fatalf("want 2 seasons, got %d: %+v", len(item.Seasons), item.Seasons)
	}
	if got := len(item.Seasons[0].Episodes); got != 2 {
		t.Errorf("season 1 should have the two numbered files, got %d", got)
	}
	if got := len(item.Seasons[1].Episodes); got != 1 {
		t.Errorf("season 2 should have one episode, got %d", got)
	}
	if e := item.Seasons[0].Episodes[0]; e.SeasonNumber != 1 || e.EpisodeNumber != 1 {
		t.Errorf("first episode = %+v", e)
	}
	for _, s := range item.Seasons {
		for _, e := range s.Episodes {
			if e.Monitored {
				t.Errorf("episodes of a manual entry start unmonitored: %+v", e)
			}
		}
	}
}

// Refresh has nothing to refresh against. Letting it run would either error
// every cycle or overwrite what the user typed.
func TestRefreshRefusesAManualEntryAndRefreshAllSkipsIt(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	ctx := context.Background()
	dir := manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Home Movies 2003")

	manual, err := svc.AddManual(ctx, ManualRequest{
		Kind: domain.KindMovie, Title: "Home Movies 2003", Path: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshItem(ctx, manual.ID); !errors.Is(err, ErrManualEntry) {
		t.Errorf("want ErrManualEntry, got %v", err)
	}

	// And the scheduled sweep steps over it rather than reporting a failure.
	if _, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, Monitored: true, Monitor: "none",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshAll(ctx); err != nil {
		t.Errorf("RefreshAll should skip manual entries silently, got %v", err)
	}
	// Still there, still manual, title untouched.
	after, err := svc.Get(ctx, manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "Home Movies 2003" || !after.IsManual() {
		t.Errorf("the entry was altered by a refresh sweep: %+v", after)
	}
}

// A rescan adds what has appeared and never removes: a missing file is the
// normal state of a monitored episode.
func TestRescanManualAddsNewEpisodesAndKeepsOldOnes(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()
	dir := manualRoot(t, svc, domain.RootKindOf(domain.KindSeries), "Tapes",
		"Tapes.S01E01.mkv")

	item, err := svc.AddManual(ctx, ManualRequest{
		Kind: domain.KindSeries, Title: "Tapes", Path: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Seasons[0].Episodes) != 1 {
		t.Fatalf("want 1 episode to start, got %d", len(item.Seasons[0].Episodes))
	}

	// A second file appears, and the first one goes away.
	if err := os.WriteFile(filepath.Join(dir, "Tapes.S01E02.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "Tapes.S01E01.mkv")); err != nil {
		t.Fatal(err)
	}

	after, err := svc.RescanManual(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Seasons) != 1 || len(after.Seasons[0].Episodes) != 2 {
		t.Fatalf("want both episodes kept, got %+v", after.Seasons)
	}
}

// Rescan is for manual entries only — a provider-backed item has metadata to
// refresh against, and reading its episodes off the disk would fight it.
func TestRescanRefusesAProviderBackedItem(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{series: []ports.SearchResult{series(10, "Severance", 2022)}}
	ctx := context.Background()

	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindSeries, TMDBID: 10, Monitored: true, Monitor: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RescanManual(ctx, item.ID); err == nil {
		t.Fatal("want a refusal for a provider-backed item")
	}
}

// Same security check adoption makes: the library must not be aimable at
// arbitrary places on the host by anyone who can post JSON.
func TestAddManualRefusesFoldersOutsideARoot(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()
	manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Real Folder")

	for _, path := range []string{"/etc", "relative/path", filepath.Join(t.TempDir(), "nowhere")} {
		if _, err := svc.AddManual(ctx, ManualRequest{
			Kind: domain.KindMovie, Title: "Anything", Path: path,
		}); err == nil {
			t.Errorf("%q should be refused", path)
		}
	}
}

// Two records for one directory is how a folder loses its files to whichever
// item wins the next scan, silently.
func TestAddManualRefusesAFolderAnotherItemHolds(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()
	dir := manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Once")

	if _, err := svc.AddManual(ctx, ManualRequest{
		Kind: domain.KindMovie, Title: "Once", Path: dir,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.AddManual(ctx, ManualRequest{
		Kind: domain.KindMovie, Title: "Once Again", Path: dir,
	})
	if !errors.Is(err, ErrFolderConflict) {
		t.Errorf("want ErrFolderConflict, got %v", err)
	}
}

func TestAddManualNeedsATitleAndAKnownKind(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()
	dir := manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Untitled")

	if _, err := svc.AddManual(ctx, ManualRequest{
		Kind: domain.KindMovie, Title: "   ", Path: dir,
	}); err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("want a complaint about the title, got %v", err)
	}
	if _, err := svc.AddManual(ctx, ManualRequest{
		Kind: "sculpture", Title: "A Thing", Path: dir,
	}); !errors.Is(err, ErrUnsupportedKind) {
		t.Errorf("want ErrUnsupportedKind, got %v", err)
	}
}

// The folder is answered for once an entry exists, so review stops offering
// it — the same pruning adoption does.
func TestAddManualDropsTheFolderFromReview(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()
	dir := manualRoot(t, svc, domain.RootKindOf(domain.KindMovie), "Unmatchable")
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Counts.Total; got != 1 {
		t.Fatalf("want the folder in review to begin with, got %d", got)
	}

	if _, err := svc.AddManual(ctx, ManualRequest{
		Kind: domain.KindMovie, Title: "Unmatchable", Path: dir,
	}); err != nil {
		t.Fatal(err)
	}
	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Counts.Total; got != 0 {
		t.Errorf("the folder should stop being offered, got %d", got)
	}
}
