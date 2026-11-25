package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// disbelievedTarget lays out the Good Trouble S02E06 case (2026-11-25): the
// primary copy's only file for the episode was measured implausible, and the
// release the search found renders to the same path. The corpus clip runs
// seconds, so against a 60-minute runtime it measures implausible again.
func disbelievedTarget(t *testing.T, tracked bool) (*Service, domain.MediaItem, importScope, sqlite.Placement) {
	t.Helper()
	svc, db, id := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	item.Runtime = 60
	ep, err := db.GetEpisodeID(ctx, id, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(item.Path, "Season 1", "Test Show - S01E01 [WEB-DL 1080p].mkv")
	if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	if tracked {
		fid, err := db.UpsertFile(ctx, id, 0, target, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep}); err != nil {
			t.Fatal(err)
		}
		if err = db.SetFileQualityFrom(ctx, fid, quality.Quality{Source: quality.SourceUnknown, Resolution: 720},
			mediainfo.ProvenanceImplausible, mediainfo.ConfidenceNone); err != nil {
			t.Fatal(err)
		}
	}
	previous, _, generation, err := fileDigestSnapshot(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if generation == "" {
		t.Skip("file generations are unavailable on this platform")
	}
	p := sqlite.Placement{
		Target: target, SHA256: "new-release-digest", PreviousSHA256: previous,
		TargetObserved: true, TargetFileGeneration: generation,
		EpisodeIDs: []int64{ep}, ItemID: id,
		Quality:    quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		Provenance: mediainfo.ProvenanceRelease,
	}
	scope := importScope{ProfileID: item.QualityProfileID, Release: "Test.Show.S01E01.1080p.WEB-DL"}
	return svc, item, scope, p
}

func TestDisbelievedFileAtTheDestinationIsReplaceable(t *testing.T) {
	svc, item, scope, p := disbelievedTarget(t, true)
	if err := svc.validateMeasuredPlacement(context.Background(), item, scope, p, false); err != nil {
		t.Fatalf("a file that still measures implausible blocked its replacement: %v", err)
	}
}

func TestDisbelievedTargetIsReMeasuredNotTakenFromTheRow(t *testing.T) {
	t.Run("now measures plausible", func(t *testing.T) {
		svc, item, scope, p := disbelievedTarget(t, true)
		item.Runtime = 0 // the runtime that made it implausible is gone
		err := svc.validateMeasuredPlacement(context.Background(), item, scope, p, false)
		if err == nil || !strings.Contains(err.Error(), "now measures plausible") {
			t.Fatalf("replaced a file the bytes no longer refute: %v", err)
		}
		if !isLibraryRefusal(err) {
			t.Error("a verdict on the library's own file is not marked library-blocked")
		}
	})
	t.Run("bytes changed after the preview", func(t *testing.T) {
		svc, item, scope, p := disbelievedTarget(t, true)
		p.TargetFileGeneration = "0:0:0:0:0"
		err := svc.validateMeasuredPlacement(context.Background(), item, scope, p, false)
		if err == nil || !strings.Contains(err.Error(), "changed after placement preview") {
			t.Fatalf("authorized bytes the preview never saw: %v", err)
		}
	})
	t.Run("payload is itself implausible", func(t *testing.T) {
		svc, item, scope, p := disbelievedTarget(t, true)
		p.Provenance = mediainfo.ProvenanceImplausible
		err := svc.validateMeasuredPlacement(context.Background(), item, scope, p, false)
		if err == nil || !strings.Contains(err.Error(), "itself implausible") {
			t.Fatalf("one disbelieved file replaced another: %v", err)
		}
		if isLibraryRefusal(err) {
			t.Error("a bad payload is the release's fault, not the library's")
		}
	})
}

// The Lil Miss Vampire case: bytes at the destination that the library does
// not track are still never overwritten, and the refusal says why.
func TestUntrackedDestinationBytesStayProtected(t *testing.T) {
	svc, item, scope, p := disbelievedTarget(t, false)
	err := svc.validateMeasuredPlacement(context.Background(), item, scope, p, false)
	if err == nil || !strings.Contains(err.Error(), "does not track the file") {
		t.Fatalf("untracked destination bytes: %v", err)
	}
	if !isLibraryRefusal(err) {
		t.Error("untracked destination bytes are not marked library-blocked")
	}
}

// The loop's bound: a release Runner completed that Curator then failed
// fetched the whole release. Before 2026-11-25 these rows spent nothing, so a
// deterministic import refusal regrabbed 6-7 full releases a night.
func TestCompletedReleasesCuratorFailedSpendTheBudget(t *testing.T) {
	svc, db, itemID := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := svc.wantableFromID(ctx, "episode:1:1:1")
	if err != nil {
		t.Skipf("fixture has no such wantable: %v", err)
	}
	fail := func(title, outcome string) {
		t.Helper()
		id, err := db.InsertDownload(ctx, sqlite.Download{
			MediaItemID: itemID, WantableIDs: []string{"episode:1:1:1"}, ReleaseTitle: title,
			Protocol: "usenet", ClientID: clients[0].ID, State: "grabbed", Size: 3 << 30,
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		row.State, row.Progress = "failed", 1
		if err := db.UpdateDownloadHandoff(ctx, row); err != nil {
			t.Fatal(err)
		}
		if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET runner_outcome=?, runner_blames_release=0 WHERE id=?`, outcome, id); err != nil {
			t.Fatal(err)
		}
	}
	// Blameless Runner failures still cost nothing.
	for i := 0; i < reGrabLimit; i++ {
		fail("Cancelled."+strings.Repeat("c", i+1), "cancelled")
	}
	if capped, _ := svc.regrabCapped(ctx, w); capped {
		t.Fatal("blameless Runner failures spent the budget")
	}
	for i := 0; i < reGrabLimit; i++ {
		if capped, _ := svc.regrabCapped(ctx, w); capped {
			t.Fatalf("capped after only %d completed-then-failed releases", i)
		}
		fail("Good.Trouble.S02E06."+strings.Repeat("m", i+1), "completed")
	}
	if capped, _ := svc.regrabCapped(ctx, w); !capped {
		t.Fatalf("%d completed releases Curator failed did not cap the want", reGrabLimit)
	}
}

// A publication refused only for what the library already holds is not
// grabbed again at once: every release would meet the same verdict, and the
// grab only spends the indexer's allowance (the nightly loop that ended in a
// 429 from nzb.life, 2026-11-25). The row still fails, naming why.
func TestLibraryBlockedPublicationIsNotGrabbedAgain(t *testing.T) {
	f := setupV2(t, nil)
	ep, err := f.db.GetEpisodeID(f.ctx, f.item, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	item, err := f.db.GetMediaItemFull(f.ctx, f.item)
	if err != nil {
		t.Fatal(err)
	}
	// A file the library tracks for the episode but has never measured:
	// "on disk, quality unverified" (ADR 0013 section 5) protects it.
	existing := filepath.Join(item.Path, "Season 1", "unverified.mkv")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := f.db.UpsertFile(f.ctx, f.item, 0, existing, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.ReplaceFileEpisodeLinks(f.ctx, fid, []int64{ep}); err != nil {
		t.Fatal(err)
	}
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-blk", "blk", map[string]string{epOne + ".mkv": "video"})
	f.completedAt(dl, pub.Path, "blk")
	row := f.row(dl.ID)
	if row.State != "failed" || !strings.Contains(row.Error, "unverified existing file") {
		t.Fatalf("row = %s %q, want failed naming the unverified file", row.State, row.Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, dl.ID); n != 0 {
		t.Fatalf("replacement grabs = %d, want none for a library-blocked refusal", n)
	}
	if last := row.Handoff; !strings.Contains(last[len(last)-1].Detail, "not grabbing again") {
		t.Fatalf("handoff = %+v, want it to say why nothing was grabbed", last[len(last)-1])
	}
	if got, _ := os.ReadFile(existing); string(got) != "keep" {
		t.Fatal("the protected file changed")
	}
}
