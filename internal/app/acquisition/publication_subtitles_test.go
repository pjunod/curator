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
	} {
		if got := subtitleBaseName(rel, video); got != want {
			t.Errorf("%s -> %q, want %q", rel, got, want)
		}
	}
}
