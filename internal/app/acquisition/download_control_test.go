package acquisition

import (
	"encoding/json"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestHeldSeasonPackSuppressesEpisodeAndPack(t *testing.T) {
	c := ports.DownloadControl{Version: 1, Lifecycle: "held"}
	raw, _ := json.Marshal(c)
	row := sqlite.Download{MediaItemID: 5, RunnerControl: string(raw), WantableIDs: []string{"season:5:1"}}
	if !heldWantOverlap(row, 5, 0, []string{"episode:5:1:5"}) {
		t.Fatal("held pack failed to suppress episode")
	}
	if heldWantOverlap(row, 5, 2, []string{"episode:5:1:5:c2"}) {
		t.Fatal("suppressed unrelated copy")
	}
	row.WantableIDs = []string{"episode:5:1:5"}
	if !heldWantOverlap(row, 5, 0, []string{"season:5:1"}) {
		t.Fatal("episode hold failed to suppress pack")
	}
}
