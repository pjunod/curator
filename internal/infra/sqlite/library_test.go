package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
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

	rf, err := db.AddRootFolder(ctx, "/library/tv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddRootFolder(ctx, "/library/tv"); !errors.Is(err, ErrDuplicate) {
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

	fileID, err := db.UpsertFile(ctx, id, "/library/tv/Test Show/S01E01E02.mkv", 1234)
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
	orphanID, err := db.UpsertFile(ctx, 0, "/library/tv/Test Show/S01E01E02.mkv", 4321)
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
