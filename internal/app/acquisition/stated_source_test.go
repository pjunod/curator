package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// ripFixture writes real 1080p container bytes under a disc-rip name that
// says nothing about the title, the episode or the quality.
func ripFixture(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", "mkv-1080p-h264-ac3-whole.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "title_t00.mkv")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A disc the owner ripped: the plan shows, and the import writes, the source
// they stated and the resolution measured from the file — and the record
// says a person set the source.
func TestManualImportOfARipUsesTheStatedSourceAndMeasuredResolution(t *testing.T) {
	svc, db, itemID := setup(t, nil, nil)
	ctx := context.Background()
	rips := t.TempDir()
	rip := ripFixture(t, rips)

	plan, err := svc.PlanItemImport(ctx, ItemImportPlanRequest{MediaItemID: itemID, QualitySource: "remux",
		Files: []ItemImportFile{{Path: rip, Season: 1, Episodes: []int{1}}}})
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Error != "" || plan[0].Quality != "Remux 1080p" || !strings.Contains(plan[0].Destination, "[Remux 1080p]") {
		t.Fatalf("plan = %+v, want Remux 1080p in the quality and the destination name", plan[0])
	}

	id, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: rips, Paths: []string{rip}, MediaItemID: itemID, QualitySource: "remux",
		EpisodeTargets: map[string]sqlite.HandoffEpisodeTarget{rip: {Season: 1, Episodes: []int{1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil || statedSourceOf(dl) != quality.SourceRemux {
		t.Fatalf("queued row %+v err %v, want the stated source kept in its handoff", dl.Handoff, err)
	}
	// Without started workers the import runs inline (importers.go).
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %+v err = %v, want the ripped episode", files, err)
	}
	got := files[0]
	if got.Path != plan[0].Destination {
		t.Errorf("placed at %q, the plan said %q", got.Path, plan[0].Destination)
	}
	if got.Quality != (quality.Quality{Source: quality.SourceRemux, Resolution: 1080}) || got.Provenance != mediainfo.ProvenanceManual {
		t.Errorf("recorded %+v provenance %q, want remux 1080p set by hand", got.Quality, got.Provenance)
	}
}

// The inline form (tests and retries) honours a stated source too.
func TestInlineManualImportRecordsTheStatedSource(t *testing.T) {
	svc, db, itemID := setup(t, nil, nil)
	ctx := context.Background()
	dir := t.TempDir()
	ep := writeFile(t, filepath.Join(dir, "Test.Show.S01E01.mkv"), "x")
	if _, err := svc.ManualImport(ctx, ManualImportRequest{Path: dir, Paths: []string{ep}, MediaItemID: itemID, QualitySource: "dvd"}); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("files = %+v err = %v", files, err)
	}
	if files[0].Quality.Source != quality.SourceDVD || files[0].Provenance != mediainfo.ProvenanceManual {
		t.Fatalf("recorded %+v provenance %q, want dvd set by hand", files[0].Quality, files[0].Provenance)
	}
}

func TestManualImportRefusesAnUnknownStatedSource(t *testing.T) {
	svc, _, itemID := setup(t, nil, nil)
	ctx := context.Background()
	dir := t.TempDir()
	ep := writeFile(t, filepath.Join(dir, "Test.Show.S01E01.mkv"), "x")
	if _, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: dir, Paths: []string{ep}, MediaItemID: itemID, QualitySource: "vhs"}); err == nil {
		t.Fatal("queued with an unknown source")
	}
	if _, err := svc.PlanItemImport(ctx, ItemImportPlanRequest{MediaItemID: itemID, QualitySource: "vhs", Files: []ItemImportFile{{Path: ep, Season: -1}}}); err == nil {
		t.Fatal("planned with an unknown source")
	}
}
