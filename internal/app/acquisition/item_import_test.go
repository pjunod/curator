package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The item page's browser lists one folder: its own media, never a nested
// release's, with the subtitles that would travel with each video.
func TestScanImportFolderListsOneLevelWithSubtitles(t *testing.T) {
	svc, _, _ := setup(t, nil, nil)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Test.Show.S01E01.1080p.WEB.mkv"), "one")
	writeFile(t, filepath.Join(dir, "a7f3c91e.mkv"), "two")
	writeFile(t, filepath.Join(dir, "Test.Show.S01E01.1080p.WEB.en.srt"), "sub")
	writeFile(t, filepath.Join(dir, "Subs", "Test.Show.S01E01.1080p.WEB", "2_English.srt"), "sub")
	writeFile(t, filepath.Join(dir, "Other.Release", "Other.Release.mkv"), "nested")
	writeFile(t, filepath.Join(dir, "Other.Release", "Other.Release.srt"), "nested sub")

	got, err := svc.ScanImportFolder(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ScannedFile{}
	for _, f := range got {
		byName[f.Name] = f
	}
	if len(got) != 2 || byName["Other.Release.mkv"].Path != "" {
		t.Fatalf("listed %+v, want exactly the two videos directly in the folder", got)
	}
	if n := byName["Test.Show.S01E01.1080p.WEB.mkv"].Subtitles; n != 2 {
		t.Fatalf("subtitles with the named episode = %d, want 2 (beside it and in Subs/<stem>/)", n)
	}
	if n := byName["a7f3c91e.mkv"].Subtitles; n != 0 {
		t.Fatalf("subtitles with the obfuscated file = %d, want 0", n)
	}
	if byName["a7f3c91e.mkv"].Publication != nil {
		t.Fatal("a loose file was reported as part of a runner delivery")
	}

	_, err = svc.ScanImportFolder(ctx, filepath.Join(dir, "missing"))
	if err == nil || !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "remote path mapping") {
		t.Fatalf("err = %v, want the path named and the path-mapping fix", err)
	}
}

// The confirm step: the exact destination, what happens to what is there,
// and how the bytes get there; a series file whose name has no episode
// must be given one.
func TestPlanItemImportNamesDestinationActionAndMode(t *testing.T) {
	svc, db, itemID := setup(t, nil, nil)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	src := writeFile(t, filepath.Join(filepath.Dir(item.Path), "incoming", "a7f3c91e.mkv"), "video")

	plan, err := svc.PlanItemImport(ctx, ItemImportPlanRequest{MediaItemID: itemID, Files: []ItemImportFile{{Path: src, Season: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0].Error, "choose the episode") {
		t.Fatalf("plan = %+v, want an episode required", plan)
	}

	plan, err = svc.PlanItemImport(ctx, ItemImportPlanRequest{MediaItemID: itemID,
		Files: []ItemImportFile{{Path: src, Season: 1, Episodes: []int{2}}}})
	if err != nil {
		t.Fatal(err)
	}
	line := plan[0]
	if line.Error != "" || !strings.HasPrefix(line.Destination, item.Path) || !strings.Contains(line.Destination, "S01E02") {
		t.Fatalf("plan = %+v, want a destination for S01E02 under %s", line, item.Path)
	}
	if line.Action != "new" || line.Mode != "hardlink" {
		t.Fatalf("plan = %+v, want new, hardlink (same filesystem, original kept)", line)
	}
	plan, err = svc.PlanItemImport(ctx, ItemImportPlanRequest{MediaItemID: itemID, RemoveOriginal: true,
		Files: []ItemImportFile{{Path: src, Season: 1, Episodes: []int{2}}}})
	if err != nil || plan[0].Mode != "rename" {
		t.Fatalf("plan = %+v (%v), want rename when removing the original", plan, err)
	}
	plan, _ = svc.PlanItemImport(ctx, ItemImportPlanRequest{MediaItemID: itemID,
		Files: []ItemImportFile{{Path: src, Season: 1, Episodes: []int{9}}}})
	if !strings.Contains(plan[0].Error, "S01E09") {
		t.Fatalf("plan = %+v, want an unknown episode refused", plan)
	}
}

// A loose file with no episode in its name lands as the chosen episode, its
// subtitle travels with it, and Remove original takes the source away once
// the library copy is in place.
func TestQueuedManualImportPlacesChosenEpisodeWithSubtitleAndRemovesOriginal(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	svc.SetRegistry(transfers.New())
	workerCtx, stop := context.WithCancel(context.Background())
	svc.StartImporters(workerCtx)
	t.Cleanup(func() { stop(); svc.WaitImporters() })
	ctx := context.Background()

	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "a7f3c91e.mkv"), "the second episode")
	sub := writeFile(t, filepath.Join(dir, "a7f3c91e.en.srt"), "1\n00:00:01,000 --> 00:00:02,000\nhi\n")

	if _, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: dir, Paths: []string{src}, MediaItemID: itemID,
		EpisodeTargets: map[string]sqlite.HandoffEpisodeTarget{filepath.Join(dir, "x.mkv"): {Season: 1, Episodes: []int{2}}}}); err == nil {
		t.Fatal("an episode for a file that was not selected was accepted")
	}
	id, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: dir, Paths: []string{src}, MediaItemID: itemID,
		EpisodeTargets: map[string]sqlite.HandoffEpisodeTarget{src: {Season: 1, Episodes: []int{2}}}, RemoveOriginal: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		dl, err := db.GetDownload(ctx, id)
		return err == nil && (dl.State == "imported" || dl.State == "failed")
	})
	waitFor(t, func() bool { return !exists(src) })
	dl, _ := db.GetDownload(ctx, id)
	if dl.State != "imported" {
		t.Fatalf("row = %s (%s), want imported", dl.State, dl.Error)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 || !strings.Contains(files[0].Path, "S01E02") {
		t.Fatalf("library files = %+v (%v), want one file placed as S01E02", files, err)
	}
	libSub := strings.TrimSuffix(files[0].Path, filepath.Ext(files[0].Path)) + ".en.srt"
	if !exists(libSub) {
		t.Fatalf("subtitle %s was not placed beside the video", libSub)
	}
	if exists(src) || exists(sub) {
		t.Fatal("Remove original left a source behind")
	}
}

// Remove original never reaches into a runner delivery: those files leave
// only by the publication's cleanup rules.
func TestRemoveOriginalRefusesARunnerDelivery(t *testing.T) {
	f := setupV2(t, nil)
	cfg := f.cfg
	cfg.ManualApproval = true
	if err := f.db.UpdateDownloadClient(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-301.runner-pack", "pack", map[string]string{"a7f3c91e.mkv": "episode two"})
	f.complete(dl, pub)
	file := filepath.Join(pub.Path, "a7f3c91e.mkv")
	if _, err := f.svc.QueueManualImport(f.ctx, ManualImportRequest{Path: pub.Path, Paths: []string{file}, MediaItemID: f.item, RemoveOriginal: true}); err == nil || !strings.Contains(err.Error(), "Resolve") {
		t.Fatalf("err = %v, want the runner delivery sent to Resolve", err)
	}
}

// A runner delivery's file is found by the folder listing and placed
// through Resolve as the episode a person chose.
func TestResolvePlaceWithChosenEpisode(t *testing.T) {
	f := setupV2(t, nil)
	cfg := f.cfg
	cfg.ManualApproval = true
	if err := f.db.UpdateDownloadClient(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-302.runner-pack", "pack", map[string]string{
		"Test.Show.S01E01.mkv": "episode one", "a7f3c91e.mkv": "episode two"})
	f.complete(dl, pub)

	listed, err := f.svc.ScanImportFolder(f.ctx, pub.Path)
	if err != nil {
		t.Fatal(err)
	}
	var obf *ScannedPublication
	for _, s := range listed {
		if s.Name == "a7f3c91e.mkv" {
			obf = s.Publication
		}
	}
	if obf == nil || obf.DownloadID != dl.ID || obf.Path != "a7f3c91e.mkv" {
		t.Fatalf("listing = %+v, want the file's runner delivery", listed)
	}
	plan, err := f.svc.PlanItemImport(f.ctx, ItemImportPlanRequest{MediaItemID: f.item,
		Files: []ItemImportFile{{Path: filepath.Join(pub.Path, "a7f3c91e.mkv"), Season: 1, Episodes: []int{2}}}})
	if err != nil || plan[0].Mode != "runner" || plan[0].Error != "" {
		t.Fatalf("plan = %+v (%v), want a runner placement", plan, err)
	}
	if err := f.svc.ResolvePublicationFileTo(f.ctx, dl.ID, obf.PublicationID, obf.Path, "place",
		PlaceTarget{MediaItemID: f.item, Season: 1, Episodes: []int{2}}); err != nil {
		t.Fatal(err)
	}
	_, files := f.publicationFiles(pub.Path)
	got := files["a7f3c91e.mkv"]
	if got.Outcome != "placed" || !strings.Contains(got.LibraryPath, "S01E02") {
		t.Fatalf("file = %+v, want placed as S01E02", got)
	}
	if err := f.svc.ResolvePublicationFileTo(f.ctx, dl.ID, obf.PublicationID, "Test.Show.S01E01.mkv", "place",
		PlaceTarget{MediaItemID: f.item, Season: 1, Episodes: []int{7}}); err == nil {
		t.Fatal("an episode the series does not have was accepted")
	}
}
