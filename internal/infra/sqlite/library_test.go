package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func sampleSeries() domain.MediaItem {
	return domain.MediaItem{
		Kind:      domain.KindSeries,
		Title:     "Test Show",
		SortTitle: "test show",
		Year:      2020,
		IDs:       domain.ExternalIDs{TMDB: 100, TVDB: 200},
		Genres:    []string{"Drama", "Sci-Fi"},
		Monitored: true,
		Seasons: []domain.Season{
			{Number: 0, Monitored: false, Episodes: []domain.Episode{
				{SeasonNumber: 0, EpisodeNumber: 1, Title: "Special", Monitored: false},
			}},
			{Number: 1, Monitored: true, Episodes: []domain.Episode{
				{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", AirDate: "2020-01-01", Monitored: true},
				{SeasonNumber: 1, EpisodeNumber: 2, Title: "Two", AirDate: "2020-01-08", Monitored: true},
			}},
		},
	}
}

func TestCreateAndGetSeriesTree(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.CreateMediaItem(ctx, sampleSeries())
	if err != nil {
		t.Fatalf("CreateMediaItem: %v", err)
	}

	got, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatalf("GetMediaItemFull: %v", err)
	}
	if got.Title != "Test Show" || got.Kind != domain.KindSeries || got.Year != 2020 {
		t.Errorf("item = %+v", got)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Drama" {
		t.Errorf("genres = %v", got.Genres)
	}
	if len(got.Seasons) != 2 {
		t.Fatalf("seasons = %d", len(got.Seasons))
	}
	if got.Seasons[0].Number != 0 || got.Seasons[0].Monitored {
		t.Errorf("specials season wrong: %+v", got.Seasons[0])
	}
	if len(got.Seasons[1].Episodes) != 2 || got.Seasons[1].Episodes[0].Title != "Pilot" {
		t.Errorf("season 1 episodes = %+v", got.Seasons[1].Episodes)
	}
}

func TestDuplicateKindTmdbRejected(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateMediaItem(ctx, sampleSeries()); err != nil {
		t.Fatal(err)
	}
	_, err := db.CreateMediaItem(ctx, sampleSeries())
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("second insert err = %v, want ErrDuplicate", err)
	}

	// Same TMDB id under a different kind is allowed (id spaces differ).
	movie := domain.MediaItem{
		Kind: domain.KindMovie, Title: "Test Movie", SortTitle: "test movie",
		Year: 2020, IDs: domain.ExternalIDs{TMDB: 100}, Monitored: true,
	}
	if _, err := db.CreateMediaItem(ctx, movie); err != nil {
		t.Errorf("movie with same tmdb id should insert: %v", err)
	}
}

func TestDuplicateKindTvdbRejectedAcrossDifferentTmdbIDs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	first := sampleSeries()
	first.IDs.TMDB = 0
	if _, err := db.CreateMediaItem(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := sampleSeries()
	second.IDs.TMDB = 131927
	if _, err := db.CreateMediaItem(ctx, second); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("shared TVDB identity should be duplicate, got %v", err)
	}
}

func TestListAndDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	seriesID, _ := db.CreateMediaItem(ctx, sampleSeries())
	db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "A Movie", SortTitle: "a movie",
		Year: 2021, IDs: domain.ExternalIDs{TMDB: 7}, Monitored: true,
	})

	all, err := db.ListMediaItems(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("ListMediaItems = %d items, err %v", len(all), err)
	}
	movies, _ := db.ListMediaItems(ctx, domain.KindMovie)
	if len(movies) != 1 || movies[0].Title != "A Movie" {
		t.Errorf("movie filter = %+v", movies)
	}

	if err := db.DeleteMediaItem(ctx, seriesID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetMediaItemFull(ctx, seriesID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete err = %v, want ErrNotFound", err)
	}
}

func TestRootFolders(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	rf, err := db.AddRootFolder(ctx, "/library/tv", domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddRootFolder(ctx, "/library/tv", domain.KindMixed); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate path err = %v, want ErrDuplicate", err)
	}
	list, _ := db.ListRootFolders(ctx)
	if len(list) != 1 || list[0].Path != "/library/tv" {
		t.Errorf("list = %+v", list)
	}
	if err := db.DeleteRootFolder(ctx, rf.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetRootFolder(ctx, rf.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete err = %v", err)
	}
}

func TestFilesAndEpisodeLinks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, _ := db.CreateMediaItem(ctx, sampleSeries())
	ep1, err := db.GetEpisodeID(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	ep2, _ := db.GetEpisodeID(ctx, id, 1, 2)

	fileID, err := db.UpsertFile(ctx, id, 0, "/library/tv/Test Show/S01E01E02.mkv", 1234)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{ep1, ep2}); err != nil {
		t.Fatal(err)
	}

	files, err := db.ListFilesForItem(ctx, id)
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %v, err %v", files, err)
	}
	if len(files[0].EpisodeIDs) != 2 {
		t.Errorf("episode links = %v", files[0].EpisodeIDs)
	}

	// HasFile is reflected on the detail view.
	item, _ := db.GetMediaItemFull(ctx, id)
	var s1 domain.Season
	for _, s := range item.Seasons {
		if s.Number == 1 {
			s1 = s
		}
	}
	if !s1.Episodes[0].HasFile || !s1.Episodes[1].HasFile {
		t.Errorf("HasFile not set: %+v", s1.Episodes)
	}

	// Upsert same path re-links to a different (unmatched) owner.
	orphanID, err := db.UpsertFile(ctx, 0, 0, "/library/tv/Test Show/S01E01E02.mkv", 4321)
	if err != nil {
		t.Fatal(err)
	}
	if orphanID != fileID {
		t.Errorf("upsert same path returned new id %d != %d", orphanID, fileID)
	}
	all, _ := db.ListAllFiles(ctx)
	if len(all) != 1 || all[0].MediaItemID != 0 || all[0].Size != 4321 {
		t.Errorf("after orphan upsert: %+v", all)
	}
}

func TestListMediaItemStats(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Series: 2 monitored aired episodes (plus an unaired one and an
	// unmonitored special), one episode has a file.
	s := sampleSeries()
	s.Seasons[1].Episodes = append(s.Seasons[1].Episodes,
		domain.Episode{SeasonNumber: 1, EpisodeNumber: 3, Title: "Future", AirDate: "2999-01-01", Monitored: true})
	seriesID, err := db.CreateMediaItem(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	ep1, err := db.GetEpisodeID(ctx, seriesID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, seriesID, 0, "/library/tv/Test Show/S01E01.mkv", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{ep1}); err != nil {
		t.Fatal(err)
	}

	// Movie with a file, and a movie without.
	movieID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Have", SortTitle: "have", IDs: domain.ExternalIDs{TMDB: 1}, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertFile(ctx, movieID, 0, "/library/movies/Have/have.mkv", 200); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Missing", SortTitle: "missing", IDs: domain.ExternalIDs{TMDB: 2}, Monitored: true,
	}); err != nil {
		t.Fatal(err)
	}

	items, err := db.ListMediaItems(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]domain.MediaItem{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	show := byTitle["Test Show"]
	if show.EpisodeCount != 2 || show.EpisodeFileCount != 1 || show.FileCount != 1 {
		t.Errorf("series stats = %d/%d files=%d, want 1 of 2 aired with 1 file",
			show.EpisodeFileCount, show.EpisodeCount, show.FileCount)
	}
	if byTitle["Have"].FileCount != 1 || byTitle["Missing"].FileCount != 0 {
		t.Errorf("movie stats: have=%d missing=%d",
			byTitle["Have"].FileCount, byTitle["Missing"].FileCount)
	}
}
