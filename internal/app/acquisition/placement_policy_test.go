package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestMeasuredDowngradeCannotPublishOrRemove(t *testing.T) {
	svc, db, id := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := db.GetEpisodeID(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(item.Path, "old.mkv")
	if err = os.WriteFile(old, []byte("protected bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, id, 0, old, 15)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQuality(ctx, fid, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}); err != nil {
		t.Fatal(err)
	}
	profile, err := db.GetProfile(ctx, item.QualityProfileID)
	if err != nil {
		t.Fatal(err)
	}
	profile.Target = quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160}
	if err = db.UpdateProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "claimed2160.mkv")
	if err = os.WriteFile(src, corpusFile(t, wholeCorpus720), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.commitPlacement(ctx, item, importScope{ProfileID: profile.ID, Release: "Test.Show.S01E01.2160p.WEB-DL"}, src, old, profile.Target, []int64{ep}, true)
	if err == nil {
		t.Fatal("measured downgrade accepted")
	}
	got, err := os.ReadFile(old)
	if err != nil || string(got) != "protected bytes" {
		t.Fatalf("old bytes changed: %q %v", got, err)
	}
}
func TestMeasuredPolicyProtectsUpgradesOffAndUnverified(t *testing.T) {
	for _, unverified := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrades off", true: "unverified"}[unverified], func(t *testing.T) {
			svc, db, id := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			item, _ := db.GetMediaItemFull(ctx, id)
			ep, _ := db.GetEpisodeID(ctx, id, 1, 1)
			old := filepath.Join(item.Path, "existing.mkv")
			if err := os.WriteFile(old, []byte("keep"), 0644); err != nil {
				t.Fatal(err)
			}
			fid, err := db.UpsertFile(ctx, id, 0, old, 4)
			if err != nil {
				t.Fatal(err)
			}
			if err = db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep}); err != nil {
				t.Fatal(err)
			}
			if !unverified {
				if err = db.SetFileQuality(ctx, fid, quality.Quality{Source: quality.SourceHDTV, Resolution: 720}); err != nil {
					t.Fatal(err)
				}
			}
			profile, _ := db.GetProfile(ctx, item.QualityProfileID)
			profile.UpgradesAllowed = false
			if err = db.UpdateProfile(ctx, profile); err != nil {
				t.Fatal(err)
			}
			p := sqlite.Placement{Source: "Test.Show.S01E01.1080p.WEB-DL.mkv", Target: filepath.Join(item.Path, "new.mkv"), Quality: profile.Target, EpisodeIDs: []int64{ep}}
			if err = svc.validateMeasuredPlacement(ctx, item, importScope{ProfileID: profile.ID}, p, true); err == nil {
				t.Fatal("protected file accepted")
			}
		})
	}
}
