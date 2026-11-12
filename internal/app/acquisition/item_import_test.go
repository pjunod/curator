package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/domain"
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
	writeFile(t, filepath.Join(dir, "Unrelated.Movie.2019.srt"), "someone else's subtitle")
	if err := os.Symlink(filepath.Join(dir, "a7f3c91e.mkv"), filepath.Join(dir, "link.mkv")); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ScanImportFolder(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ScannedFile{}
	for _, f := range got {
		byName[f.Name] = f
	}
	if len(got) != 3 || byName["Other.Release.mkv"].Path != "" {
		t.Fatalf("listed %+v, want the two videos directly in the folder and the symlink", got)
	}
	if !strings.Contains(byName["link.mkv"].Skipped, "symlink") || byName["a7f3c91e.mkv"].Skipped != "" {
		t.Fatalf("symlink = %+v, want it listed as skipped with the reason", byName["link.mkv"])
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
	waitFor(t, func() bool {
		row, err := db.GetDownload(ctx, id)
		_, done := lastStep(row, stepOriginalsDone)
		return err == nil && done
	})
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

// manualApprovalV2 is the v2 fixture with a client that holds completed
// deliveries for a person, so a test decides what is placed.
func manualApprovalV2(t *testing.T) *v2Fixture {
	t.Helper()
	f := setupV2(t, nil)
	cfg := f.cfg
	cfg.ManualApproval = true
	if err := f.db.UpdateDownloadClient(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	return f
}

// addSeries adds a second series to the library.
func (f *v2Fixture) addSeries(title string) int64 {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), title)
	_ = os.MkdirAll(dir, 0o755)
	id, err := f.db.CreateMediaItem(f.ctx, domain.MediaItem{
		Kind: domain.KindSeries, Title: title, SortTitle: strings.ToLower(title), Year: 2021,
		IDs: domain.ExternalIDs{TMDB: 777}, Monitored: true, Path: dir,
		Seasons: []domain.Season{{Number: 1, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 1, EpisodeNumber: 1, Title: "One", Monitored: true, AirDate: "2021-01-01"},
			{SeasonNumber: 1, EpisodeNumber: 2, Title: "Two", Monitored: true, AirDate: "2021-01-08"},
		}}},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// B1: two files of one runner delivery, picked together, are placed in one
// admission and one import, each as its chosen episode.
func TestPlacePublicationFilesPlacesTwoFilesTogether(t *testing.T) {
	f := manualApprovalV2(t)
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-302.runner-pack", "pack", map[string]string{
		"a7f3c91e.mkv": "episode one", "b81c22d0.mkv": "episode two"})
	f.complete(dl, pub)

	listed, err := f.svc.ScanImportFolder(f.ctx, pub.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].Publication == nil || listed[0].Publication.DownloadID != dl.ID {
		t.Fatalf("listing = %+v, want both files with their runner delivery", listed)
	}
	pubID := listed[0].Publication.PublicationID
	plan, err := f.svc.PlanItemImport(f.ctx, ItemImportPlanRequest{MediaItemID: f.item, Files: []ItemImportFile{
		{Path: filepath.Join(pub.Path, "a7f3c91e.mkv"), Season: 1, Episodes: []int{1}},
		{Path: filepath.Join(pub.Path, "b81c22d0.mkv"), Season: 1, Episodes: []int{2}}}})
	if err != nil || plan[0].Mode != "runner" || plan[0].Error != "" || plan[1].Error != "" || plan[0].Note != "" {
		t.Fatalf("plan = %+v (%v), want two runner placements into the delivery's own item", plan, err)
	}
	if err := f.svc.PlacePublicationFiles(f.ctx, dl.ID, pubID, PlaceRequest{MediaItemID: f.item, Files: []PlaceFile{
		{Path: "a7f3c91e.mkv", Season: 1, Episodes: []int{1}}, {Path: "b81c22d0.mkv", Season: 1, Episodes: []int{2}}}}); err != nil {
		t.Fatal(err)
	}
	_, files := f.publicationFiles(pub.Path)
	if a, b := files["a7f3c91e.mkv"], files["b81c22d0.mkv"]; a.Outcome != "placed" || !strings.Contains(a.LibraryPath, "S01E01") ||
		b.Outcome != "placed" || !strings.Contains(b.LibraryPath, "S01E02") {
		t.Fatalf("files = %+v / %+v, want both placed as their chosen episodes", a, b)
	}
	if err := f.svc.PlacePublicationFiles(f.ctx, dl.ID, pubID, PlaceRequest{Files: []PlaceFile{{Path: "a7f3c91e.mkv", Season: 1, Episodes: []int{7}}}}); err == nil {
		t.Fatal("a file already placed was accepted again")
	}
}

// B1: a placement while this delivery's import holds the download is refused
// before anything is written.
func TestPlacePublicationFilesRefusedWhileImportRunsWritesNothing(t *testing.T) {
	f := manualApprovalV2(t)
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-303.runner-pack", "pack", map[string]string{"a7f3c91e.mkv": "episode one"})
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	before := f.row(dl.ID)
	unlock := f.svc.lockDownload(dl.ID)
	err := f.svc.PlacePublicationFiles(f.ctx, dl.ID, p.ID, PlaceRequest{Files: []PlaceFile{{Path: "a7f3c91e.mkv", Season: 1, Episodes: []int{1}}}})
	unlock()
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("err = %v, want the running import named", err)
	}
	after := f.row(dl.ID)
	if len(after.Handoff) != len(before.Handoff) {
		t.Fatalf("a refused placement wrote the trace: %+v", after.Handoff[len(before.Handoff):])
	}
	if got, _ := f.publicationFiles(pub.Path); got.State != p.State {
		t.Fatalf("publication = %s, want it untouched (%s)", got.State, p.State)
	}
}

// B2/B3: a person's placement is honoured by every later pass. Here the pass
// after the placement is an ordinary unapproved one (a failure returned the
// publication, or the process restarted before the queued job ran): it
// places exactly the chosen file, as the chosen episode, into the item the
// delivery was moved to, and leaves the rest unresolved.
func TestPlacementSurvivesAnUnapprovedPassAndMovesTheDelivery(t *testing.T) {
	f := manualApprovalV2(t)
	other := f.addSeries("Other Show")
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-304.runner-pack", "pack", map[string]string{
		"Test.Show.S01E01.mkv": "really other show two", "Test.Show.S01E02.mkv": "test show two"})
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)

	plan, err := f.svc.PlanItemImport(f.ctx, ItemImportPlanRequest{MediaItemID: other,
		Files: []ItemImportFile{{Path: filepath.Join(pub.Path, "Test.Show.S01E01.mkv"), Season: 1, Episodes: []int{2}}}})
	if err != nil || plan[0].Error != "" || !strings.Contains(plan[0].Note, "Test Show") {
		t.Fatalf("plan = %+v (%v), want the move to this item said", plan, err)
	}

	// What PlacePublicationFiles commits, without the job it then queues.
	row := f.row(dl.ID)
	abs := filepath.Join(pub.Path, "Test.Show.S01E01.mkv")
	row.MediaItemID, row.CopyID = other, 0
	row.Handoff = append(row.Handoff, sqlite.HandoffEntry{Step: stepManualQueued, Placement: true, Paths: []string{abs},
		TargetMediaItemID: other, EpisodeTargets: map[string]sqlite.HandoffEpisodeTarget{abs: {Season: 1, Episodes: []int{2}}}})
	if err := f.db.CommitPublicationPlacement(f.ctx, row, p.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.enqueuePublicationImport(f.ctx, f.row(dl.ID), f.cfg, p.ID)

	_, files := f.publicationFiles(pub.Path)
	got := files["Test.Show.S01E01.mkv"]
	if got.Outcome != "placed" || !strings.Contains(got.LibraryPath, "Other Show") || !strings.Contains(got.LibraryPath, "S01E02") {
		t.Fatalf("file = %+v, want placed into Other Show as S01E02", got)
	}
	if rest := files["Test.Show.S01E02.mkv"]; rest.Outcome == "placed" {
		t.Fatalf("an unselected file was placed: %+v", rest)
	}
	// Placed now: the delivery may not move again.
	if err := f.svc.PlacePublicationFiles(f.ctx, dl.ID, p.ID, PlaceRequest{MediaItemID: f.item,
		Files: []PlaceFile{{Path: "Test.Show.S01E02.mkv", Season: 1, Episodes: []int{2}}}}); err == nil || !strings.Contains(err.Error(), "stay with that title") {
		t.Fatalf("err = %v, want the move back refused", err)
	}
}

// B3: the fence counts placements a failed or partial attempt committed,
// not only files a publication recorded as placed; and a refused move
// writes nothing.
func TestPlacementFenceCountsCommittedPlacements(t *testing.T) {
	f := manualApprovalV2(t)
	other := f.addSeries("Other Show")
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-305.runner-pack", "pack", map[string]string{"a7f3c91e.mkv": "episode"})
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES('fence-test','/lib/x.mkv','committed',json_object('download_id',?,'source_generation',?),0)`, dl.ID, dl.GenerationKey); err != nil {
		t.Fatal(err)
	}
	before := f.row(dl.ID)
	err := f.svc.PlacePublicationFiles(f.ctx, dl.ID, p.ID, PlaceRequest{MediaItemID: other, Files: []PlaceFile{{Path: "a7f3c91e.mkv", Season: 1, Episodes: []int{1}}}})
	if err == nil || !strings.Contains(err.Error(), "stay with that title") {
		t.Fatalf("err = %v, want the move refused", err)
	}
	if after := f.row(dl.ID); after.MediaItemID != before.MediaItemID || len(after.Handoff) != len(before.Handoff) {
		t.Fatalf("a refused move changed the row: item %d→%d, trace %d→%d", before.MediaItemID, after.MediaItemID, len(before.Handoff), len(after.Handoff))
	}
}

// startedService runs the importer workers for a test.
func startedService(t *testing.T) (*Service, *sqlite.DB, int64) {
	t.Helper()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	svc.SetRegistry(transfers.New())
	workerCtx, stop := context.WithCancel(context.Background())
	svc.StartImporters(workerCtx)
	t.Cleanup(func() { stop(); svc.WaitImporters() })
	return svc, db, itemID
}

func waitState(t *testing.T, db *sqlite.DB, id int64, states ...string) sqlite.Download {
	t.Helper()
	var dl sqlite.Download
	waitFor(t, func() bool {
		var err error
		dl, err = db.GetDownload(context.Background(), id)
		if err != nil {
			return false
		}
		for _, s := range states {
			if dl.State == s {
				return true
			}
		}
		return false
	})
	return dl
}

func lastStep(dl sqlite.Download, step string) (sqlite.HandoffEntry, bool) {
	for i := len(dl.Handoff) - 1; i >= 0; i-- {
		if dl.Handoff[i].Step == step {
			return dl.Handoff[i], true
		}
	}
	return sqlite.HandoffEntry{}, false
}

// S6: a retry after a failed attempt places the file as the chosen episode
// again: the choice is read from the admitted selection, not the name.
func TestManualImportRetryRereadsChosenEpisode(t *testing.T) {
	svc, db, itemID := startedService(t)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)
	// The library folder is a file: the first attempt cannot place anything.
	if err := os.RemoveAll(item.Path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, item.Path, "not a folder")
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "Test.Show.S01E01.mkv"), "really the second episode")
	id, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: dir, Paths: []string{src}, MediaItemID: itemID,
		EpisodeTargets: map[string]sqlite.HandoffEpisodeTarget{src: {Season: 1, Episodes: []int{2}}}})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, db, id, "failed")
	_ = os.Remove(item.Path)
	if err := os.MkdirAll(item.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	// The failed worker lets go of the row just after recording the failure.
	waitFor(t, func() bool { return svc.ImportNow(ctx, id) == nil })
	// The retry leaves the row failed until the import moves it (v0.46.2):
	// wait for the retry's own outcome, after its import_queued step.
	var dl sqlite.Download
	waitFor(t, func() bool {
		dl, _ = db.GetDownload(ctx, id)
		if dl.State == "imported" {
			return true
		}
		n := len(dl.Handoff)
		return dl.State == "failed" && n > 0 && dl.Handoff[n-1].Step == "failed" && func() bool {
			for i := n - 1; i >= 0; i-- {
				if dl.Handoff[i].Step == "import_queued" {
					return true
				}
			}
			return false
		}()
	})
	files, _ := db.ListFilesForItem(ctx, itemID)
	if dl.State != "imported" || len(files) != 1 || !strings.Contains(files[0].Path, "S01E02") {
		t.Fatalf("row %s (%s), files %+v: want the retry to place S01E02 as chosen, not S01E01 from the name", dl.State, dl.Error, files)
	}
}

// S3: Remove original never takes a file from the library's own folders.
func TestRemoveOriginalRefusedInsideALibraryFolder(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)
	src := writeFile(t, filepath.Join(item.Path, "extra", "Test.Show.S01E02.mkv"), "episode")
	plan, err := svc.PlanItemImport(ctx, ItemImportPlanRequest{MediaItemID: itemID, RemoveOriginal: true,
		Files: []ItemImportFile{{Path: src, Season: 1, Episodes: []int{2}}}})
	if err != nil || !strings.Contains(plan[0].Error, "inside the library folder") {
		t.Fatalf("plan = %+v (%v), want Remove original refused", plan, err)
	}
	if _, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: filepath.Dir(src), Paths: []string{src}, MediaItemID: itemID, RemoveOriginal: true}); err == nil || !strings.Contains(err.Error(), "inside the library folder") {
		t.Fatalf("err = %v, want Remove original refused", err)
	}
}

// S1/S2/S6: the journaled removal takes a source only while it is the
// chosen file and its library copy matches (a copy across volumes is
// compared by bytes); otherwise it keeps it and says why on the row. A
// removal journaled before a restart is finished by the next start.
func TestOriginalRemovalChecksIdentityAndResumes(t *testing.T) {
	svc, db, itemID := startedService(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "Test.Show.S01E01.mkv"), "episode one")
	id, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: dir, Paths: []string{src}, MediaItemID: itemID})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, db, id, "imported")

	// A removal journaled by a process that stopped before doing it: one
	// copied source (bytes equal), one changed since it was chosen, one whose
	// library copy differs.
	lib := t.TempDir()
	identity := func(p string) string {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return removalIdentity(info)
	}
	copied := writeFile(t, filepath.Join(dir, "copied.mkv"), "same bytes")
	copiedLib := writeFile(t, filepath.Join(lib, "copied.mkv"), "same bytes")
	changed := writeFile(t, filepath.Join(dir, "changed.mkv"), "before")
	changedID := identity(changed)
	writeFile(t, changed, "after, longer")
	changedLib := writeFile(t, filepath.Join(lib, "changed.mkv"), "after, longer")
	differs := writeFile(t, filepath.Join(dir, "differs.mkv"), "source bytes")
	differsLib := writeFile(t, filepath.Join(lib, "differs.mkv"), "other bytes!")
	if err := db.AppendHandoffEntry(ctx, id, sqlite.HandoffEntry{Step: stepOriginalsPending, Removals: []sqlite.HandoffRemoval{
		{Source: copied, Library: copiedLib, Identity: identity(copied)},
		{Source: changed, Library: changedLib, Identity: changedID},
		{Source: differs, Library: differsLib, Identity: identity(differs)},
	}}); err != nil {
		t.Fatal(err)
	}
	svc.resumeOriginalRemovals(ctx)

	if exists(copied) || !exists(changed) || !exists(differs) {
		t.Fatalf("copied gone=%v changed kept=%v differs kept=%v", !exists(copied), exists(changed), exists(differs))
	}
	dl, _ := db.GetDownload(ctx, id)
	done, ok := lastStep(dl, stepOriginalsDone)
	if !ok || !strings.Contains(done.Detail, "removed 1") || !strings.Contains(done.Detail, "changed after it was chosen") || !strings.Contains(done.Detail, "differs") {
		t.Fatalf("result = %+v, want one removed and both kept originals named with their reasons", done)
	}
	// Finished: a later start does nothing more.
	svc.resumeOriginalRemovals(ctx)
	dl, _ = db.GetDownload(ctx, id)
	if n := strings.Count(fmt.Sprint(dl.Handoff), stepOriginalsDone); n != 1 {
		t.Fatalf("removal recorded %d times", n)
	}
}

// S6: across volumes the plan says copy, verify and remove.
func TestPlanSaysCopyRemoveAcrossVolumes(t *testing.T) {
	svc, _, itemID := setup(t, nil, nil)
	previous := sameStorage
	sameStorage = func(string, string) bool { return false }
	t.Cleanup(func() { sameStorage = previous })
	src := writeFile(t, filepath.Join(t.TempDir(), "Test.Show.S01E02.mkv"), "episode")
	for remove, want := range map[bool]string{false: "copy", true: "copy_remove"} {
		plan, err := svc.PlanItemImport(context.Background(), ItemImportPlanRequest{MediaItemID: itemID, RemoveOriginal: remove,
			Files: []ItemImportFile{{Path: src, Season: 1, Episodes: []int{2}}}})
		if err != nil || plan[0].Mode != want {
			t.Fatalf("remove=%v plan = %+v (%v), want %s", remove, plan, err, want)
		}
	}
}

// S6: a copy or edition is honoured by the plan and by a runner placement.
func TestCopyTargetingPlanAndPlacement(t *testing.T) {
	f := manualApprovalV2(t)
	copyDir := t.TempDir()
	copyID, err := f.db.AddMediaCopy(f.ctx, domain.MediaCopy{MediaItemID: f.item, Name: "720p", QualityProfileID: 1, Path: copyDir, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-306.runner-pack", "pack", map[string]string{"a7f3c91e.mkv": "episode one"})
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	plan, err := f.svc.PlanItemImport(f.ctx, ItemImportPlanRequest{MediaItemID: f.item, CopyID: copyID,
		Files: []ItemImportFile{{Path: filepath.Join(pub.Path, "a7f3c91e.mkv"), Season: 1, Episodes: []int{1}}}})
	if err != nil || !strings.HasPrefix(plan[0].Destination, copyDir) {
		t.Fatalf("plan = %+v (%v), want a destination in the copy's folder", plan, err)
	}
	if _, err := f.svc.PlanItemImport(f.ctx, ItemImportPlanRequest{MediaItemID: f.item, CopyID: 999,
		Files: []ItemImportFile{{Path: filepath.Join(pub.Path, "a7f3c91e.mkv")}}}); !errors.Is(err, ErrInvalidImportPlan) {
		t.Fatalf("err = %v, want an unknown copy refused as invalid", err)
	}
	if err := f.svc.PlacePublicationFiles(f.ctx, dl.ID, p.ID, PlaceRequest{MediaItemID: f.item, CopyID: copyID,
		Files: []PlaceFile{{Path: "a7f3c91e.mkv", Season: 1, Episodes: []int{1}}}}); err != nil {
		t.Fatal(err)
	}
	_, files := f.publicationFiles(pub.Path)
	if got := files["a7f3c91e.mkv"]; got.Outcome != "placed" || !strings.HasPrefix(got.LibraryPath, copyDir) {
		t.Fatalf("file = %+v, want placed in the copy's folder", got)
	}
}

// Minor/S5: in a loose folder a subtitle beside a lone video travels only
// when its name says it belongs to it; a cancelled import places none.
func TestLooseSubtitlesMatchByNameAndStopOnCancel(t *testing.T) {
	svc, _, _ := setup(t, nil, nil)
	dir := t.TempDir()
	video := writeFile(t, filepath.Join(dir, "Lone.Movie.2020.mkv"), "video")
	writeFile(t, filepath.Join(dir, "Lone.Movie.2020.en.srt"), "ours")
	writeFile(t, filepath.Join(dir, "Unrelated.Film.srt"), "theirs")
	if n := subtitleCounts(dir)["Lone.Movie.2020.mkv"]; n != 1 {
		t.Fatalf("subtitles counted = %d, want only the one named after the video", n)
	}
	lib := filepath.Join(t.TempDir(), "Lone Movie (2020).mkv")
	writeFile(t, lib, "video")
	result := ImportResult{Files: []FileOutcome{{Imported: true, SourcePath: video, Path: lib}}}
	placed := svc.placeManualSubtitles(context.Background(), result)
	if len(placed) != 1 || !strings.HasSuffix(placed[0].Library, "Lone Movie (2020).en.srt") {
		t.Fatalf("placed = %+v, want only the matching subtitle", placed)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = os.Remove(placed[0].Library)
	if again := svc.placeManualSubtitles(cancelled, result); len(again) != 0 || exists(placed[0].Library) {
		t.Fatalf("a cancelled import placed %+v", again)
	}
}
