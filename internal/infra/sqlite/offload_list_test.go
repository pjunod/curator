package sqlite

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

// The library list flags a show whose offloaded season has no episode rows
// (none known yet): it counts as an Offloaded show even though no episode
// carries the flag.
func TestListMediaItemsCountsOffloadedSeasonsWithoutEpisodes(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	id, err := d.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Empty", SortTitle: "empty", Year: 2020, IDs: domain.ExternalIDs{TMDB: 5},
		Seasons: []domain.Season{{Number: 1, Monitored: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE seasons SET offloaded_at=1 WHERE media_item_id=?`, id); err != nil {
		t.Fatal(err)
	}
	items, err := d.ListMediaItems(ctx, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %d", err, len(items))
	}
	if items[0].OffloadedEpisodes != 0 || items[0].OffloadedSeasons != 1 {
		t.Fatalf("episodes=%d seasons=%d, want 0 and 1", items[0].OffloadedEpisodes, items[0].OffloadedSeasons)
	}
}
