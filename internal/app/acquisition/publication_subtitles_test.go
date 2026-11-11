package acquisition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// importPublication completes dl with a publication of files and returns the
// publication's files by path once the import (and its cleanup) ran.
func (f *v2Fixture) importPublication(dl sqlite.Download, name string, files map[string]string) (sqlite.Publication, map[string]sqlite.PublicationFile) {
	f.t.Helper()
	pub := f.publish(name, strings.ReplaceAll(name, ".", "-"), files)
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, ok, err := f.db.PublicationByPath(f.ctx, pub.Path)
	if err != nil || !ok {
		f.t.Fatalf("publication: ok=%v err=%v", ok, err)
	}
	rows, err := f.db.ListPublicationFiles(f.ctx, p.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]sqlite.PublicationFile{}
	for _, r := range rows {
		out[r.Path] = r
	}
	return p, out
}

func libraryStem(t *testing.T, f sqlite.PublicationFile) string {
	t.Helper()
	if f.Outcome != "placed" || f.LibraryPath == "" {
		t.Fatalf("video = %+v, want placed", f)
	}
	return strings.TrimSuffix(f.LibraryPath, filepath.Ext(f.LibraryPath))
}

func wantFile(t *testing.T, path, body string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("%s = %q (%v), want %q", path, got, err, body)
	}
}

// One video and a Subs/ folder of numbered language names: every subtitle
// goes with the video, named <stem>.<lang>, a second English one gets its own
// name, flags are kept, an unknown language adds nothing, and the
// publication is cleaned up with nothing left for Resolve.
func TestSingleVideoSubtitlesArePlacedWithLanguages(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	p, files := f.importPublication(dl, "Test.Show.S01E01.job-401.runner-subs", map[string]string{
		epOne + ".mkv":         "video",
		"Subs/2_English.srt":   "english",
		"Subs/3_Spanish.srt":   "spanish",
		"Subs/4_English.srt":   "english sdh?",
		"Subs/eng.forced.srt":  "forced",
		"Subs/en.hi.srt":       "hearing impaired",
		"Subs/track1.srt":      "unknown",
		epOne + ".nfo":         "nfo",
		"Screens/screen01.jpg": "jpg",
	})
	if f.row(dl.ID).State != "imported" || (p.State != "removed" && p.State != "imported") {
		t.Fatalf("row %s publication %s (%s)", f.row(dl.ID).State, p.State, p.Error)
	}
	stem := libraryStem(t, files[epOne+".mkv"])
	for rel, want := range map[string]struct{ path, body string }{
		"Subs/2_English.srt":  {stem + ".en.srt", "english"},
		"Subs/4_English.srt":  {stem + ".en.2.srt", "english sdh?"},
		"Subs/3_Spanish.srt":  {stem + ".es.srt", "spanish"},
		"Subs/eng.forced.srt": {stem + ".en.forced.srt", "forced"},
		"Subs/en.hi.srt":      {stem + ".en.sdh.srt", "hearing impaired"},
		"Subs/track1.srt":     {stem + ".srt", "unknown"},
	} {
		if got := files[rel]; got.Outcome != "placed" || got.LibraryPath != want.path || got.PlacedSHA256 != got.SHA256 {
			t.Errorf("%s = %+v, want placed at %s", rel, got, want.path)
			continue
		}
		wantFile(t, want.path, want.body)
	}
	for _, rel := range []string{epOne + ".nfo", "Screens/screen01.jpg"} {
		if got := files[rel]; got.Outcome != "discarded" || got.DiscardReason != "rejected" {
			t.Errorf("%s = %+v, want discarded{rejected}", rel, got)
		}
	}
	for rel, got := range files {
		if got.Outcome == "unresolved" || got.Outcome == "pending" {
			t.Errorf("%s left for a person: %+v", rel, got)
		}
	}
	// The cleanup counted the subtitles: the publication is gone, the
	// library copies stay.
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := f.db.PublicationByPath(f.ctx, p.Path); p.State != "removed" || exists(p.Path) {
		t.Fatalf("publication = %s (%s), want removed", p.State, p.Error)
	}
	wantFile(t, stem+".en.srt", "english")
}

// A season pack: each subtitle goes with the episode its folder or name
// identifies; one that identifies none is discarded, not left for Resolve.
func TestSeasonPackSubtitlesFollowTheirEpisode(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(0, seasonPack)
	_, files := f.importPublication(dl, "Test.Show.S01.job-402.runner-pack", map[string]string{
		epOne + ".mkv":                     "one",
		epTwo + ".mkv":                     "two",
		"Subs/" + epOne + "/2_English.srt": "one en",
		"Subs/" + epTwo + "/3_French.srt":  "two fr",
		epTwo + ".de.srt":                  "two de",
		"Subs/Extras/2_English.srt":        "nobody's",
	})
	one, two := libraryStem(t, files[epOne+".mkv"]), libraryStem(t, files[epTwo+".mkv"])
	for rel, want := range map[string]string{
		"Subs/" + epOne + "/2_English.srt": one + ".en.srt",
		"Subs/" + epTwo + "/3_French.srt":  two + ".fr.srt",
		epTwo + ".de.srt":                  two + ".de.srt",
	} {
		if got := files[rel]; got.Outcome != "placed" || got.LibraryPath != want {
			t.Errorf("%s = %+v, want placed at %s", rel, got, want)
		}
	}
	if got := files["Subs/Extras/2_English.srt"]; got.Outcome != "discarded" || !strings.Contains(got.Reason, "matches no video") {
		t.Errorf("unmatched subtitle = %+v, want discarded naming why", got)
	}
}

// An .idx and its .sub are one subtitle: both go, under one name.
func TestVobSubPairTravelsTogether(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	_, files := f.importPublication(dl, "Test.Show.S01E01.job-403.runner-vobsub", map[string]string{
		epOne + ".mkv":     "video",
		"Subs/English.idx": "index",
		"Subs/English.sub": "bitmap",
	})
	stem := libraryStem(t, files[epOne+".mkv"])
	if got := files["Subs/English.idx"]; got.LibraryPath != stem+".en.idx" {
		t.Fatalf("idx = %+v", got)
	}
	if got := files["Subs/English.sub"]; got.LibraryPath != stem+".en.sub" {
		t.Fatalf("sub = %+v", got)
	}
	wantFile(t, stem+".en.idx", "index")
	wantFile(t, stem+".en.sub", "bitmap")
}

func TestSubtitleBaseNameReadsLanguageFromNameThenFolder(t *testing.T) {
	video := publicationVideo{rel: "Show.S01E01.mkv", library: "/lib/Show/Season 1/Show - S01E01 - Pilot.mkv"}
	base := "Show - S01E01 - Pilot"
	for rel, want := range map[string]string{
		"Show.S01E01.srt":              base,
		"Show.S01E01.eng.srt":          base + ".en",
		"Show.S01E01.it.srt":           base + ".it",
		"It.Was.Something.srt":         base, // a title word is not Italian
		"Subs/German/1.srt":            base + ".de",
		"Subs/Show.S01E01/Danish.srt":  base + ".da",
		"Show.S01E01.pt-BR.forced.srt": base + ".pt.forced",
		"zz_unknownlang.srt":           base,
		"hi.srt":                       base + ".hi",
		"Show.S01E01.hi.srt":           base + ".hi",
		"Show.S01E01.en.hi.srt":        base + ".en.sdh",
		"Show.S01E01.fr-CA.srt":        base + ".fr",
		"Movie.Title.Ice.srt":          base, // a title word, not Icelandic
		"The.English.Patient.srt":      base, // only the last token can be the language
		"Subs/3_Spanish.srt":           base + ".es",
	} {
		if got := subtitleBaseName(rel, video); got != want {
			t.Errorf("%s -> %q, want %q", rel, got, want)
		}
	}
}

// An upgrade that replaces a video takes the subtitles Curator placed for it
// (by their placement record) with it, and places the new release's own; a
// subtitle Curator did not place stays.
func TestUpgradeReplacesThePlacedSubtitlesOfTheOldVideo(t *testing.T) {
	f := setupV2(t, nil)
	const oldRelease = "Test.Show.S01E01.720p.HDTV.x264-OLD"
	first := f.grab(1, oldRelease)
	_, files := f.importPublication(first, "Test.Show.S01E01.job-701.runner-old", map[string]string{
		oldRelease + ".mkv": "old video", oldRelease + ".en.srt": "old english"})
	oldStem := libraryStem(t, files[oldRelease+".mkv"])
	wantFile(t, oldStem+".en.srt", "old english")
	foreign := oldStem + ".fr.srt" // put there by someone else
	if err := os.WriteFile(foreign, []byte("not curator's"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := f.grab(1, epOne)
	_, files = f.importPublication(second, "Test.Show.S01E01.job-702.runner-new", map[string]string{
		epOne + ".mkv": "new video", "Subs/2_English.srt": "new english"})
	if f.row(second.ID).State != "imported" {
		t.Fatalf("upgrade row = %s (%s)", f.row(second.ID).State, f.row(second.ID).Error)
	}
	newStem := libraryStem(t, files[epOne+".mkv"])
	if newStem == oldStem {
		t.Fatal("fixture: the upgrade landed at the old name")
	}
	if exists(oldStem + ".mkv") {
		t.Fatal("the old video was not replaced")
	}
	if exists(oldStem + ".en.srt") {
		t.Fatal("the replaced video's Curator-placed subtitle was left behind")
	}
	wantFile(t, foreign, "not curator's")
	wantFile(t, newStem+".en.srt", "new english")
	var outcome string
	if err := f.db.R.QueryRowContext(f.ctx, `SELECT outcome||'/'||discard_reason FROM publication_files WHERE path=?`, oldRelease+".en.srt").Scan(&outcome); err != nil || outcome != "discarded/superseded" {
		t.Fatalf("the removed subtitle's record = %q (%v), want discarded/superseded", outcome, err)
	}
}

// An upgrade that lands at the same name as the old video replaces the old
// subtitles Curator placed under that name, including languages the new
// release does not carry.
func TestInPlaceReplacementRetiresTheOldPlacedSubtitles(t *testing.T) {
	f := setupV2(t, nil)
	first := f.grab(1, epOne)
	_, files := f.importPublication(first, "Test.Show.S01E01.job-703.runner-a", map[string]string{
		epOne + ".mkv": "video", epOne + ".de.srt": "german"})
	stem := libraryStem(t, files[epOne+".mkv"])
	wantFile(t, stem+".de.srt", "german")
	// The decision that the new placement replaced the old file at this name
	// is the placement's; here the subtitle step sees a video it placed now.
	p, _, _ := f.db.PublicationByPath(f.ctx, filepath.Join(f.root, "Test.Show.S01E01.job-703.runner-a"))
	item, err := f.db.GetMediaItemFull(f.ctx, f.item)
	if err != nil {
		t.Fatal(err)
	}
	other := sqlite.Publication{ID: p.ID + 1000, Path: t.TempDir()}
	if _, err := f.svc.decidePublicationSidecars(f.ctx, item, other, ports.Manifest{Files: []ports.ManifestFile{{Path: epOne + ".mkv"}}},
		map[string]string{epOne + ".mkv": stem + ".mkv"}, nil, map[string]bool{epOne + ".mkv": true}); err != nil {
		t.Fatal(err)
	}
	if exists(stem + ".de.srt") {
		t.Fatal("the old release's subtitle survived an in-place replacement")
	}
}

// Sidecars an earlier build left for Resolve in an already imported
// publication (migration 0076 makes them pending) are decided by the cleanup
// pass: the subtitle goes beside the video already placed, the rest is
// discarded, the placed video is untouched, and the directory is retired.
func TestLeftoverSidecarsOfAnImportedPublicationAreDecided(t *testing.T) {
	f := setupV2(t, nil)
	f.holdCleanup()
	dl := f.grab(1, epOne)
	p, files := f.importPublication(dl, "Test.Show.S01E01.job-704.runner-left", map[string]string{
		epOne + ".mkv": "video", "Subs/2_English.srt": "english", epOne + ".nfo": "nfo"})
	stem := libraryStem(t, files[epOne+".mkv"])
	// What an earlier build recorded: both sidecars unresolved, nothing placed.
	if err := os.Remove(stem + ".en.srt"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Subs/2_English.srt", epOne + ".nfo"} {
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publication_files SET outcome='pending', discard_reason='', library_path='', placed_sha256='', reason='' WHERE publication_id=? AND path=?`, p.ID, rel); err != nil {
			t.Fatal(err)
		}
	}
	video := files[epOne+".mkv"]
	before, err := os.Stat(video.LibraryPath)
	if err != nil {
		t.Fatal(err)
	}
	f.sweep()
	p, files = f.publicationFiles(p.Path)
	if got := files["Subs/2_English.srt"]; got.Outcome != "placed" || got.LibraryPath != stem+".en.srt" {
		t.Fatalf("leftover subtitle = %+v, want placed beside the video", got)
	}
	if got := files[epOne+".nfo"]; got.Outcome != "discarded" || got.DiscardReason != "rejected" {
		t.Fatalf("leftover nfo = %+v, want discarded", got)
	}
	wantFile(t, stem+".en.srt", "english")
	after, err := os.Stat(video.LibraryPath)
	if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the placed video was touched")
	}
	if p.State != "removed" || exists(p.Path) {
		t.Fatalf("publication = %s (%s), want the directory retired", p.State, p.Error)
	}
}

// A subtitle that cannot be read from the publication is discarded with the
// reason; the import finishes.
func TestUnreadableSubtitleIsDiscardedNotRetried(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-705.runner-gone", "gone", map[string]string{epOne + ".mkv": "video", epOne + ".en.srt": "english"})
	if err := os.Remove(filepath.Join(pub.Path, epOne+".en.srt")); err != nil {
		t.Fatal(err)
	}
	f.holdCleanup()
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, files := f.publicationFiles(pub.Path)
	if f.row(dl.ID).State != "imported" || files[epOne+".mkv"].Outcome != "placed" {
		t.Fatalf("row %s publication %s (%s), want imported", f.row(dl.ID).State, p.State, p.Error)
	}
	if got := files[epOne+".en.srt"]; got.Outcome != "discarded" || !strings.Contains(got.Reason, "cannot be read") {
		t.Fatalf("missing subtitle = %+v, want discarded with the reason", got)
	}
}

// A subtitle Curator did not place is never overwritten: the new one takes
// the next free name.
func TestSubtitleNeverOverwritesAForeignFile(t *testing.T) {
	f := setupV2(t, nil)
	item, err := f.db.GetMediaItemFull(f.ctx, f.item)
	if err != nil {
		t.Fatal(err)
	}
	stem := filepath.Join(item.Path, "Season 1", "Test Show - S01E01 - Pilot [WEB-DL 1080p]")
	if err := os.MkdirAll(filepath.Dir(stem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stem+".en.srt", []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := f.grab(1, epOne)
	_, files := f.importPublication(dl, "Test.Show.S01E01.job-706.runner-foreign", map[string]string{epOne + ".mkv": "video", "Subs/2_English.srt": "ours"})
	if got := libraryStem(t, files[epOne+".mkv"]); got != stem {
		t.Fatalf("fixture: video at %s, want %s", got, stem)
	}
	wantFile(t, stem+".en.srt", "someone else's")
	wantFile(t, stem+".en.2.srt", "ours")
	if got := files["Subs/2_English.srt"]; got.LibraryPath != stem+".en.2.srt" {
		t.Fatalf("subtitle = %+v", got)
	}
}

// A leftover sidecar whose publication's video was deleted since takes no
// place beside it; one of a publication with no download row is decided
// too. Neither stays pending.
func TestLeftoverSidecarsWithoutTheirVideoAreDiscarded(t *testing.T) {
	f := setupV2(t, nil)
	f.holdCleanup()
	dl := f.grab(1, epOne)
	p, files := f.importPublication(dl, "Test.Show.S01E01.job-707.runner-stale", map[string]string{
		epOne + ".mkv": "video", epOne + ".en.srt": "english"})
	stem := libraryStem(t, files[epOne+".mkv"])
	if err := os.Remove(stem + ".en.srt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(files[epOne+".mkv"].LibraryPath); err != nil { // deleted since
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publication_files SET outcome='pending', library_path='', placed_sha256='', reason='' WHERE publication_id=? AND path=?`, p.ID, epOne+".en.srt"); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(f.root, "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO publications(id,generation,kind,path,state,discovered_at,updated_at) VALUES(900,'o','completed',?,'imported',1,1)`, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO publication_files(publication_id,path,bytes,sha256,outcome,updated_at) VALUES(900,'x.nfo',1,'d','pending',1)`); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	_, files = f.publicationFiles(p.Path)
	if got := files[epOne+".en.srt"]; got.Outcome != "discarded" || exists(stem+".en.srt") {
		t.Fatalf("subtitle of a deleted video = %+v (placed: %v)", got, exists(stem+".en.srt"))
	}
	_, orphanFiles := f.publicationFiles(orphan)
	if got := orphanFiles["x.nfo"]; got.Outcome != "discarded" {
		t.Fatalf("a download-less publication's leftover = %+v, want discarded", got)
	}
}
