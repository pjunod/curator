package acquisition

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestSeriesAutoSearchFallsBackToEpisodes(t *testing.T) {
	for _, packAvailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "episode fallback", true: "pack covers episodes"}[packAvailable], func(t *testing.T) {
			client := &fakeClient{}
			releases := []ports.Release{
				{Title: "Test.Show.S01E01.1080p.WEB-DL-GRP", DownloadURL: "ep1", Protocol: "torrent", Indexer: "idx"},
				{Title: "Test.Show.S01E02.1080p.WEB-DL-GRP", DownloadURL: "ep2", Protocol: "torrent", Indexer: "idx"},
			}
			if packAvailable {
				releases = append(releases, ports.Release{Title: "Test.Show.S01.1080p.WEB-DL-GRP", DownloadURL: "pack", Protocol: "torrent", Indexer: "idx"})
			}
			svc, db, id := setup(t, releases, client)
			ctx := context.Background()
			if _, err := db.W.ExecContext(ctx, `UPDATE episodes SET air_date = '2020-01-01' WHERE media_item_id = ?`, id); err != nil {
				t.Fatal(err)
			}
			out, err := svc.AutoSearchItem(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if packAvailable {
				want = 1
			}
			if out.Grabbed != want || len(client.added) != want {
				t.Fatalf("out=%+v, downloads=%v", out, client.added)
			}
			again, err := svc.AutoSearchItem(ctx, id)
			if err != nil || again.Grabbed != 0 || len(client.added) != want {
				t.Fatalf("duplicate downloads: %+v, %v", again, err)
			}
		})
	}
}

func TestSeriesAutoSearchOngoingSeasonAndSelections(t *testing.T) {
	for _, next := range []struct {
		name, date string
		monitored  bool
	}{
		{"future", "2999-01-01", true}, {"unknown", "", true}, {"unselected", "2020-01-01", false},
	} {
		t.Run(next.name, func(t *testing.T) {
			svc, db, id := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			idx := &recordingIndexer{releases: []ports.Release{
				{Title: "Test.Show.S01.1080p.WEB-DL-GRP", DownloadURL: "pack", Protocol: "torrent", Indexer: "idx"},
				{Title: "Test.Show.S01E01.1080p.WEB-DL-GRP", DownloadURL: "ep1", Protocol: "torrent", Indexer: "idx"},
			}}
			svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return idx }
			item, _ := db.GetMediaItemFull(ctx, id)
			item.Seasons[0].Episodes[0].AirDate = "2020-01-01"
			item.Seasons[0].Episodes[1].AirDate = next.date
			if err := db.UpdateMediaItemMetadata(ctx, id, item); err != nil {
				t.Fatal(err)
			}
			if err := db.SetEpisodeMonitored(ctx, id, item.Seasons[0].Episodes[1].ID, next.monitored); err != nil {
				t.Fatal(err)
			}
			out, err := svc.AutoSearchItem(ctx, id)
			if err != nil || out.Grabbed != 1 || len(out.Targets) != 1 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if out.Targets[0].Grabbed != "Test.Show.S01E01.1080p.WEB-DL-GRP" {
				t.Fatalf("grabbed pack through episode search: %+v", out)
			}
			for _, q := range idx.asked() {
				if q == "Test Show S01" {
					t.Fatal("searched for a pack of a partial season")
				}
			}
		})
	}
}

func TestWantedExcludesKnownFutureDates(t *testing.T) {
	svc, db, id := seriesSetup(t, &fakeClient{})
	ctx := context.Background()
	if _, err := db.W.ExecContext(ctx, `UPDATE episodes SET air_date = '2999-01-01' WHERE media_item_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	wanted, err := svc.Wanted(ctx)
	if err != nil || len(wanted) != 0 {
		t.Fatalf("wanted=%v err=%v", wanted, err)
	}
	// A cached yesterday cannot hide today's newly aired episodes.
	if _, err := db.W.ExecContext(ctx, `UPDATE episodes SET air_date = '2020-01-01' WHERE media_item_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	svc.wanted.day = "2000-01-01"
	wanted, err = svc.Wanted(ctx)
	if err != nil || len(wanted) != 2 {
		t.Fatalf("wanted=%v err=%v", wanted, err)
	}
}
