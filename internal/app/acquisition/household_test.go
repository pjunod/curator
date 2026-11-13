package acquisition

import (
	"context"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"os"
	"path/filepath"
	"testing"
)

func TestHouseholdImportRejectsExtraEpisodeAndArrivingPrimary(t *testing.T) {
	s, db, id := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	root, err := db.AddRootFolderWithPresence(ctx, filepath.Dir(item.Path), domain.KindSeries, "volume")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_items SET root_folder_id=? WHERE id=?`, root.ID, id); err != nil {
		t.Fatal(err)
	}
	item, err = db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	identity, generation, err := db.PresenceRootBinding(ctx, root.ID, root.Path)
	if err != nil {
		t.Fatal(err)
	}
	ep1, err := db.GetEpisodeID(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	ep2, err := db.GetEpisodeID(ctx, id, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	s.SetHouseholdVerifier(func(context.Context, int64) error { return nil })
	origin := &domain.HouseholdOrigin{ItemID: id, Kind: item.Kind, TMDBID: item.IDs.TMDB, EpisodeIDs: []int64{ep1}, Path: item.Path, RootID: root.ID, RootPath: root.Path, RootIdentity: identity, RootGeneration: generation}
	scope := importScope{Household: origin, CopyID: 0, SourceGeneration: "generation", Transfer: "transfer"}
	placement := sqlite.Placement{Target: filepath.Join(item.Path, "new.mkv"), EpisodeIDs: []int64{ep1, ep2}, SHA256: "new"}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("mixed episode file exceeded approval")
	}
	placement.EpisodeIDs = []int64{ep2}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("extra episode admitted")
	}
	placement.EpisodeIDs = []int64{ep1}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err != nil {
		t.Fatal("exact empty primary scope refused", err)
	}
	arrived := filepath.Join(item.Path, "arrived.mkv")
	if err = os.WriteFile(arrived, []byte("keep arrived primary"), 0600); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, id, 0, arrived, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep1}); err != nil {
		t.Fatal(err)
	}
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("household request implicitly upgraded arriving primary")
	}
	bytes, err := os.ReadFile(arrived)
	if err != nil || string(bytes) != "keep arrived primary" {
		t.Fatal("arrival changed", err)
	}
	origin.RootGeneration++
	if err = s.validateHouseholdImport(ctx, item, scope, placement); err == nil {
		t.Fatal("changed volume accepted")
	}
}
