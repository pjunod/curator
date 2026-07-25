package library

import (
	"context"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/ports"
)

// seedMovie adds a movie and optionally puts a file of a given quality on it.
func seedMovie(t *testing.T, svc *Service, tmdbID int64, title string, profileID int64, qs ...quality.Quality) domain.MediaItem {
	t.Helper()
	ctx := context.Background()
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: tmdbID, QualityProfileID: profileID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range qs {
		fileID, err := svc.db.UpsertFile(ctx, item.ID, 0, title+string(rune('a'+i))+".mkv", 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.db.SetFileQuality(ctx, fileID, q); err != nil {
			t.Fatal(err)
		}
	}
	return item
}

func byTitle(items []domain.MediaItem, title string) domain.MediaItem {
	for _, m := range items {
		if m.Title == title {
			return m
		}
	}
	return domain.MediaItem{}
}

// The three questions a poster grid should answer without a click: is
// anything here, is it good enough, is monarr still looking.
func TestListGradesEachItemAgainstItsProfile(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1, "At Target", 2020),
		movie(2, "Still Hunting", 2021),
		movie(3, "Nothing Yet", 2022),
	}}
	ctx := context.Background()

	uhd := int64(3) // Ultra-HD: cutoff 2160p, upgrades allowed
	seedMovie(t, svc, 1, "At Target", uhd, quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160})
	seedMovie(t, svc, 2, "Still Hunting", uhd, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080})
	seedMovie(t, svc, 3, "Nothing Yet", uhd)

	items, err := svc.List(ctx, domain.KindMovie)
	if err != nil {
		t.Fatal(err)
	}

	if got := byTitle(items, "At Target"); got.Upgrade != domain.UpgradeMet {
		t.Errorf("2160p against a 2160p cutoff is done, got %q", got.Upgrade)
	}
	hunting := byTitle(items, "Still Hunting")
	if hunting.Upgrade != domain.UpgradeSeeking {
		t.Errorf("1080p against a 2160p cutoff is still wanted, got %q", hunting.Upgrade)
	}
	if hunting.Quality.Resolution != 1080 || hunting.QualityTarget.Resolution != 2160 {
		t.Errorf("want 1080p -> 2160p, got %v -> %v", hunting.Quality, hunting.QualityTarget)
	}
	if got := byTitle(items, "Nothing Yet"); got.Upgrade != domain.UpgradeMissing {
		t.Errorf("no files at all is missing, got %q", got.Upgrade)
	}
}

// The weakest file decides, not the best. A series nine-tenths upgraded is
// not finished, and a card claiming otherwise is the reason someone opens
// every item to check.
func TestTheWeakestFileDecidesTheState(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(1, "Mixed Bag", 2020)}}

	seedMovie(t, svc, 1, "Mixed Bag", 3,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160},
		quality.Quality{Source: quality.SourceHDTV, Resolution: 720},
	)

	items, err := svc.List(context.Background(), domain.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	got := byTitle(items, "Mixed Bag")
	if got.Quality.Resolution != 720 {
		t.Errorf("want the weakest file to set the state, got %v", got.Quality)
	}
	if got.Upgrade != domain.UpgradeSeeking {
		t.Errorf("want seeking, got %q", got.Upgrade)
	}
}

// A profile with upgrades off is not "still looking" — saying so would
// promise something that will never happen.
func TestUpgradesOffReadsAsCappedRatherThanSeeking(t *testing.T) {
	p := quality.Profile{
		ID: 99, Target: quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160},
		UpgradesAllowed: false,
	}
	m := domain.MediaItem{
		Kind: domain.KindMovie, FileCount: 1,
		Quality:         quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		QualityVerified: true,
	}
	if got := upgradeState(m, p); got != domain.UpgradeCapped {
		t.Errorf("want capped, got %q", got)
	}

	p.UpgradesAllowed = true
	if got := upgradeState(m, p); got != domain.UpgradeSeeking {
		t.Errorf("want seeking once upgrades are allowed, got %q", got)
	}
}

// A file whose quality was never recorded is still a file. Reporting it as
// missing had the Quality row contradict the Files table two panels down:
// an adopted file whose name carries no quality tag ("A Good Day to Die
// Hard.mkv") has nothing recorded, which is not the same as not existing.
func TestAFileWithNoRecordedQualityIsNotMissing(t *testing.T) {
	p := quality.Profile{ID: 1, Target: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}}
	m := domain.MediaItem{Kind: domain.KindMovie, FileCount: 1} // quality zero
	if got := upgradeState(m, p); got != domain.UpgradeUnknown {
		t.Errorf("want unknown rather than missing, got %q", got)
	}

	empty := domain.MediaItem{Kind: domain.KindMovie}
	if got := upgradeState(empty, p); got != domain.UpgradeMissing {
		t.Errorf("no files really is missing, got %q", got)
	}
}

// Series count episodes with files, not raw file rows — the same split the
// completeness pill uses, so the two cannot disagree.
func TestSeriesEmptinessIsMeasuredInEpisodes(t *testing.T) {
	p := quality.Profile{ID: 1, Target: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}}
	m := domain.MediaItem{
		Kind: domain.KindSeries, FileCount: 3, EpisodeFileCount: 0,
		Quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
	}
	if got := upgradeState(m, p); got != domain.UpgradeMissing {
		t.Errorf("no episode has a file, so the series is missing; got %q", got)
	}
	m.EpisodeFileCount = 1
	if got := upgradeState(m, p); got != domain.UpgradeMet {
		t.Errorf("want met once an episode has a file, got %q", got)
	}
}

// The state monarr is in for most adopted libraries: files present, names
// carrying no quality tag. It must read as "not recorded", never as "no
// files" — the Files table is right there.
func TestAdoptedFileWithNoQualityTagInItsNameIsOnDiskButUnknown(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(1, "Die Hard", 2013)}}
	ctx := context.Background()

	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 1, Monitored: true, Monitor: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the shape of a real adopted file: no resolution, no source.
	if _, err := svc.db.UpsertFile(ctx, item.ID, 0, "/m/A Good Day to Die Hard.mkv", 1); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Upgrade == domain.UpgradeMissing {
		t.Error("a file with no recorded quality is still a file")
	}
	if got.Upgrade != domain.UpgradeUnknown {
		t.Errorf("want unknown, got %q", got.Upgrade)
	}
	if got.FileCount != 1 {
		t.Errorf("the file must still be counted, got %d", got.FileCount)
	}

	// And in the list view, which computes the same thing a different way.
	items, err := svc.List(ctx, domain.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Upgrade == domain.UpgradeMissing {
		t.Errorf("list view disagrees with the item page: %q", items[0].Upgrade)
	}
}
