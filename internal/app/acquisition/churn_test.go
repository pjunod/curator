package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// TestUnknownQualityOnDiskIsNotHunted is the named regression for Failure B in
// docs/plan-quality-truth.md — the silent one, the bug nobody would have
// reported as a bug.
//
// The chain it pins, link by link: a file whose quality was never determined
// was omitted from FileQualities, which made BestQualityForItem report "no
// known quality", which made buildWanted treat the item as MISSING, which made
// the decision engine accept any allowed release, which made RSS sync grab a
// replacement — and then the import path, seeing no current quality, judged
// itself not an upgrade and left the 17 GB original in place. Two files, no
// story, no error anywhere.
//
// The fix is one line in wants(): files on disk with no determinable quality
// are not wanted. This test is here so that line cannot be quietly undone.
func TestUnknownQualityOnDiskIsNotHunted(t *testing.T) {
	t.Run("movie", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, movieID := autoSetup(t, nil, client)
		ctx := context.Background()
		item, err := db.GetMediaItemFull(ctx, movieID)
		if err != nil {
			t.Fatal(err)
		}

		// The screenshot: a big file with a name that says nothing, and a
		// quality record we never managed to fill in.
		path := filepath.Join(item.Path, "A Good Day to Die Hard.mkv")
		if err := os.WriteFile(path, []byte("seventeen gigabytes, honest"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.UpsertFile(ctx, movieID, 0, path, 17<<30); err != nil {
			t.Fatal(err)
		}

		wanted, err := svc.Wanted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range wanted {
			if w.MediaItemID() == movieID {
				t.Fatalf("a movie with a file on disk is being hunted as missing: %s", w.ID())
			}
		}

		// And to be sure the test is testing something: with the file gone,
		// it IS wanted. A genuinely missing item must still hunt.
		if err := db.DeleteFile(ctx, fileIDFor(t, db, movieID, path)); err != nil {
			t.Fatal(err)
		}
		svc.InvalidateWanted()
		wanted, err = svc.Wanted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !huntedFor(wanted, movieID) {
			t.Fatal("a movie with nothing on disk stopped being wanted; the fix went too far")
		}
	})

	t.Run("episode", func(t *testing.T) {
		client := &fakeClient{}
		svc, db, _ := autoSetup(t, nil, client)
		ctx := context.Background()

		seriesID, err := db.CreateMediaItem(ctx, domain.MediaItem{
			Kind: domain.KindSeries, Title: "Test Show", SortTitle: "test show",
			Year: 2020, Monitored: true, Path: t.TempDir(),
			Seasons: []domain.Season{{
				Number: 1, Monitored: true,
				Episodes: []domain.Episode{
					{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", Monitored: true, AirDate: "2020-01-01"},
					{SeasonNumber: 1, EpisodeNumber: 2, Title: "Two", Monitored: true, AirDate: "2020-01-08"},
				},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		item, err := db.GetMediaItemFull(ctx, seriesID)
		if err != nil {
			t.Fatal(err)
		}
		// Episode 1 has a file with no recorded quality; episode 2 has nothing.
		path := filepath.Join(item.Path, "episode one.mkv")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		fileID, err := db.UpsertFile(ctx, seriesID, 0, path, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		ep1, err := db.GetEpisodeID(ctx, seriesID, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{ep1}); err != nil {
			t.Fatal(err)
		}

		wanted, err := svc.Wanted(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var hunted []string
		for _, w := range wanted {
			if ep, ok := w.(domain.EpisodeWantable); ok && ep.Item == seriesID {
				hunted = append(hunted, string(ep.ID()))
				if ep.Episode == 1 {
					t.Error("episode 1 has a file on disk and is being hunted as missing")
				}
			}
		}
		if len(hunted) != 1 {
			t.Errorf("wanted episodes = %v, want only the one with no file", hunted)
		}
	})
}

// TestUnverifiedSourceAtTargetResolutionStopsHunting is the don't-churn rule
// (ADR 0013 §5) at the level people experience it: a probed 1080p file whose
// SOURCE monarr could only guess at is finished, not a replacement candidate.
// Auto-replacing a possibly-perfect file on a medium-confidence inference is
// the one move a tool sharing a disk with somebody's collection cannot make.
func TestUnverifiedSourceAtTargetResolutionStopsHunting(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t, nil, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)

	path := filepath.Join(item.Path, "Test Movie.mkv")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, movieID, 0, path, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	// Measured 1080p, source inferred as HDTV at medium confidence: below the
	// profile's WEB-DL 1080p target on the source axis, but not by anything
	// anyone measured.
	if err := db.SetFileQualityFrom(ctx, fileID,
		quality.Quality{Source: quality.SourceHDTV, Resolution: 1080},
		mediainfo.ProvenanceProbe, mediainfo.ConfidenceMedium); err != nil {
		t.Fatal(err)
	}

	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if huntedFor(wanted, movieID) {
		t.Error("a 1080p file with a guessed source is being hunted for replacement")
	}

	// The same quality from a source we DO trust is a legitimate upgrade
	// target: HDTV 1080p really is below WEB-DL 1080p, and we know it.
	if err := db.SetFileQualityFrom(ctx, fileID,
		quality.Quality{Source: quality.SourceHDTV, Resolution: 1080},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()
	wanted, err = svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !huntedFor(wanted, movieID) {
		t.Error("a verified HDTV 1080p under a WEB-DL 1080p target should still be hunted")
	}
}

// TestGrabsAreCappedAtTheTargetResolution is Failure C: under the old model a
// MISSING item grabbed the highest-ranked allowed release, unbounded above, so
// the profile formerly called "Any" would pull an 80 GB 2160p remux for an
// item it would have declared finished at WEB-DL 1080p.
func TestGrabsAreCappedAtTheTargetResolution(t *testing.T) {
	client := &fakeClient{}
	releases := []ports.Release{
		rel("Test.Movie.2024.2160p.BluRay.REMUX-BIG", 100),
		rel("Test.Movie.2024.1080p.WEB-DL-FINE", 50),
		rel("Test.Movie.2024.720p.WEB-DL-SMALL", 10),
	}
	svc, _, movieID := autoSetup(t, releases, client)
	ctx := context.Background()

	if err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatalf("adds = %v, want exactly one grab", client.added)
	}
	got := client.added[0]
	if got == "http://dl/Test.Movie.2024.2160p.BluRay.REMUX-BIG" {
		t.Fatal("grabbed 2160p under a 1080p profile — the target must cap what gets grabbed")
	}
	if got != "http://dl/Test.Movie.2024.1080p.WEB-DL-FINE" {
		t.Errorf("grabbed %q, want the best release AT the target resolution", got)
	}
}

func huntedFor(wanted []domain.Wantable, itemID int64) bool {
	for _, w := range wanted {
		if w.MediaItemID() == itemID {
			return true
		}
	}
	return false
}

func fileIDFor(t *testing.T, db *sqlite.DB, itemID int64, path string) int64 {
	t.Helper()
	records, err := db.FileQualityRecords(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Path == path {
			return r.FileID
		}
	}
	t.Fatalf("no file record for %s", path)
	return 0
}
