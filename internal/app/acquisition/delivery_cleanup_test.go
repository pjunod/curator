package acquisition

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestReplacementPackFileWithSelectedSiblingOrUnknownEpisodeIsNotDisposableExcess(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := db.GetProfile(ctx, item.QualityProfileID)
	if err != nil {
		t.Fatal(err)
	}
	selectedEpisode := item.Seasons[0].Episodes[0].ID
	for _, tc := range []struct {
		name     string
		episodes []int
	}{
		{name: "selected sibling", episodes: []int{1, 2}},
		{name: "unknown episode", episodes: []int{2, 99}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, callErr := svc.importEpisodeFile(ctx, item, importScope{Planned: true, ReplacementOperationID: "replace-one-episode", AllowedEpisodeIDs: []int64{selectedEpisode}}, profile,
				map[int64]episodeState{}, filepath.Join(t.TempDir(), "Test.Show.S01E01E02.1080p.WEB-DL.mkv"),
				parser.Parsed{Season: 1, Episodes: tc.episodes}, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, "Test.Show.S01E01E02.1080p.WEB-DL", false)
			if callErr == nil {
				t.Fatal("indivisible file unexpectedly imported outside its selected scope")
			}
			if errors.Is(callErr, errReplacementFileOutsideScope) {
				t.Fatalf("unsafe or mixed source was classified as disposable excess: %v", callErr)
			}
		})
	}
}

func mustDeliveryReceipt(t *testing.T, db *sqlite.DB, receiptID int64) sqlite.DeliveryReceipt {
	t.Helper()
	r, err := db.DeliveryReceiptByID(context.Background(), receiptID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
