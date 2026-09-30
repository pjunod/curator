package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/domain"
)

func TestMonitoringPolicyAPI(t *testing.T) {
	h, db := newLibraryServer(t, stubProvider{configured: true})
	id, err := db.CreateMediaItem(context.Background(), domain.MediaItem{
		Kind: domain.KindSeries, Title: "Show", Monitored: true,
		Seasons: []domain.Season{{Number: 1, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 1, EpisodeNumber: 1, Monitored: true, AirDate: "2020-01-01"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/api/v1/library/%d", id)
	for _, mode := range []string{"none", "future", "new_seasons", "latest", "all"} {
		rr := do(t, h, "PATCH", url, fmt.Sprintf(`{"monitor":%q}`, mode))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", mode, rr.Code, rr.Body.String())
		}
		var got apigen.MediaItemDetail
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Monitor == nil || string(*got.Monitor) != mode {
			t.Fatalf("mode missing: %+v", got)
		}
		if got.Seasons[0].Episodes[0].Monitored != (mode == "all" || mode == "latest") {
			t.Fatalf("selection wrong for %s", mode)
		}
	}
	rr := do(t, h, "PATCH", url, `{"monitor":"typo","monitored":false}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode: %d %s", rr.Code, rr.Body.String())
	}
	got, err := db.GetMediaItemFull(context.Background(), id)
	if err != nil || !got.Monitored || got.Monitor != "all" {
		t.Fatalf("invalid edit was not atomic: %+v %v", got, err)
	}
}
