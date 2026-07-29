package quality_test

import (
	"testing"

	"github.com/pjunod/monarr/internal/domain/quality"
)

func q(s quality.Source, res int) quality.Quality {
	return quality.Quality{Source: s, Resolution: res}
}

func ptr(v quality.Quality) *quality.Quality { return &v }

// TestProfileLattice enumerates the decision surface of ADR 0014 §2: every
// combination of where the current file sits relative to the target, with and
// without a floor, with and without a verified source, plus books at
// resolution 0.
//
// A table rather than prose because this is the one part of the system where
// being subtly wrong is expensive in a way nobody notices for weeks: too
// permissive and monarr deletes files it should have kept, too strict and it
// stops hunting things the user is still waiting for.
func TestProfileLattice(t *testing.T) {
	target1080 := quality.Profile{
		ID: 1, Name: "1080p", Target: q(quality.SourceWEBDL, 1080), UpgradesAllowed: true,
	}
	floored := quality.Profile{
		ID: 2, Name: "HD-1080p", Target: q(quality.SourceWEBDL, 1080),
		Floor: ptr(q(quality.SourceHDTV, 1080)), UpgradesAllowed: true,
	}
	ebook := quality.Profile{
		ID: 4, Name: "Ebook", Target: q(quality.SourceEPUB, 0), UpgradesAllowed: true,
	}

	t.Run("Met", func(t *testing.T) {
		cases := []struct {
			name     string
			profile  quality.Profile
			current  quality.Quality
			verified bool
			want     bool
			why      string
		}{
			{"below target resolution", target1080, q(quality.SourceBluray, 720), true, false,
				"a 720p Bluray is not a 1080p anything"},
			{"at target resolution, below target source", target1080, q(quality.SourceHDTV, 1080), true, false,
				""},
			{"at target exactly", target1080, q(quality.SourceWEBDL, 1080), true, true, ""},
			{"at target resolution, better source", target1080, q(quality.SourceRemux, 1080), true, true,
				"a remux is not something to replace with the WEB-DL you asked for"},
			{"above target resolution", target1080, q(quality.SourceHDTV, 2160), true, true,
				"nobody wants their 4K replaced by the 1080p target"},
			{"unverified source at target resolution", target1080, q(quality.SourceHDTV, 1080), false, true,
				"the don't-churn rule: right size, guessed source, leave it alone"},
			{"unverified source BELOW target resolution", target1080, q(quality.SourceWEBDL, 720), false, false,
				"resolution is measured, so being below it is never a guess"},
			{"floor does not affect met", floored, q(quality.SourceWEBDL, 1080), true, true,
				"a floor bounds what to grab, not what counts as finished"},
			{"book at target format", ebook, q(quality.SourceEPUB, 0), true, true, ""},
			{"book below target format", ebook, q(quality.SourceMOBI, 0), true, false, ""},
			{"book above target format", ebook, q(quality.SourceEPUB, 0), true, true, ""},
			{"unverified book format", ebook, q(quality.SourcePDF, 0), false, true,
				"resolution is 0 on both sides, so the cap is vacuous and unverified wins"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.profile.Met(tc.current, tc.verified); got != tc.want {
					t.Errorf("Met(%v, verified=%v) = %v, want %v%s",
						tc.current, tc.verified, got, tc.want, because(tc.why))
				}
			})
		}
	})

	t.Run("Acceptable", func(t *testing.T) {
		cases := []struct {
			name    string
			profile quality.Profile
			release quality.Quality
			want    bool
			why     string
		}{
			{"at the target", target1080, q(quality.SourceWEBDL, 1080), true, ""},
			{"above the target resolution", target1080, q(quality.SourceRemux, 2160), false,
				"THE fix: a missing item under this profile can no longer pull an 80 GB remux"},
			{"above the target source, same resolution", target1080, q(quality.SourceRemux, 1080), true,
				"refusing a better file at the resolution you asked for helps nobody"},
			{"below the target, no floor", target1080, q(quality.SourceHDTV, 480), true,
				"without a floor, something beats nothing"},
			{"below the floor", floored, q(quality.SourceBluray, 720), false, ""},
			{"at the floor", floored, q(quality.SourceHDTV, 1080), true, ""},
			{"book format under a video profile", target1080, q(quality.SourceEPUB, 0), false,
				"the vocabularies share a field but never compete"},
			{"a cinema recording", target1080, q(quality.SourceCAM, 1080), false,
				"a recording of a screening is not a copy of the film, at any resolution"},
			{"a telesync", target1080, q(quality.SourceTelesync, 720), false, ""},
			{"video quality under a book profile", ebook, q(quality.SourceWEBDL, 1080), false, ""},
			{"book at target", ebook, q(quality.SourceEPUB, 0), true, ""},
			{"book below target", ebook, q(quality.SourcePDF, 0), true,
				"no floor set, so a worse format is still better than no book"},
			{"audiobook under an ebook profile", ebook, q(quality.SourceM4B, 0), false,
				"an M4B is not a better EPUB; it answers a different question"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.profile.Acceptable(tc.release); got != tc.want {
					t.Errorf("Acceptable(%v) = %v, want %v%s",
						tc.release, got, tc.want, because(tc.why))
				}
			})
		}
	})

	t.Run("Upgrade", func(t *testing.T) {
		cases := []struct {
			name     string
			profile  quality.Profile
			release  quality.Quality
			current  quality.Quality
			verified bool
			want     bool
			why      string
		}{
			{"better and still below target", target1080,
				q(quality.SourceWEBDL, 1080), q(quality.SourceHDTV, 1080), true, true, ""},
			{"better but the target is already met", target1080,
				q(quality.SourceRemux, 1080), q(quality.SourceWEBDL, 1080), true, false,
				"once done, stop -- that is what a target means"},
			{"better but above the target resolution", target1080,
				q(quality.SourceWEBDL, 2160), q(quality.SourceHDTV, 720), true, false, ""},
			{"a sidegrade", target1080,
				q(quality.SourceHDTV, 1080), q(quality.SourceHDTV, 1080), true, false, ""},
			{"worse", target1080,
				q(quality.SourceHDTV, 720), q(quality.SourceWEBDL, 1080), true, false, ""},
			{"better, but the source on disk was only guessed", target1080,
				q(quality.SourceWEBDL, 1080), q(quality.SourceHDTV, 1080), false, false,
				"the don't-churn rule reaching the upgrade path"},
			{"below the floor is not an upgrade even when better", floored,
				q(quality.SourceRemux, 720), q(quality.SourceHDTV, 480), true, false, ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := tc.profile.Upgrade(tc.release, tc.current, tc.verified)
				if got != tc.want {
					t.Errorf("Upgrade(%v over %v, verified=%v) = %v, want %v%s",
						tc.release, tc.current, tc.verified, got, tc.want, because(tc.why))
				}
			})
		}
	})
}

// TestDefaultProfilesSaySomething: the seeded set is the only set most users
// will ever have, and "Any" is exactly what happens when a default is allowed
// to have no opinion.
func TestDefaultProfiles(t *testing.T) {
	profiles := quality.DefaultProfiles()
	if len(profiles) != 5 {
		t.Fatalf("expected 5 seeded profiles, got %d", len(profiles))
	}
	byID := map[int64]quality.Profile{}
	for _, p := range profiles {
		byID[p.ID] = p
		if p.Name == "Any" {
			t.Error(`"Any" is retired: a hunting profile with no opinion is not a thing`)
		}
		if p.Target.Source == "" || p.Target.Source == quality.SourceUnknown {
			t.Errorf("profile %q has no target to hunt toward", p.Name)
		}
		if p.Sentence() == "" {
			t.Errorf("profile %q cannot describe itself", p.Name)
		}
	}
	if byID[1].Name != "1080p" || byID[3].Name != "4K" {
		t.Errorf("ids 1 and 3 should be 1080p and 4K, got %q and %q", byID[1].Name, byID[3].Name)
	}
	// The sentence IS the spec, so it has to read like one.
	if got := byID[1].Sentence(); got != "hunts the best release up to WEB-DL 1080p, then stops" {
		t.Errorf("1080p sentence = %q", got)
	}
	if got := byID[2].Sentence(); got != "hunts the best release up to WEB-DL 1080p, then stops; never below HDTV 1080p" {
		t.Errorf("HD-1080p sentence = %q", got)
	}
}

// TestAllowedUnder pins the compat synthesis (ADR 0014 §6): consumers that
// only speak allowed-lists get one derived from the target, so the shim can
// never describe a profile that behaves differently.
func TestAllowedUnder(t *testing.T) {
	p := quality.Profile{Target: q(quality.SourceWEBDL, 1080), UpgradesAllowed: true}
	allowed := p.AllowedUnder()
	if len(allowed) == 0 {
		t.Fatal("no allowed qualities synthesized")
	}
	for i, a := range allowed {
		if !p.Acceptable(a) {
			t.Errorf("synthesized %v that the profile would reject", a)
		}
		if i > 0 && quality.Rank(allowed[i-1]) > quality.Rank(a) {
			t.Errorf("allowed list is not worst-to-best at index %d", i)
		}
		if a.Resolution > 1080 {
			t.Errorf("synthesized %v above the target resolution", a)
		}
	}
	if !contains(allowed, q(quality.SourceRemux, 1080)) {
		t.Error("a 1080p remux is acceptable under a WEB-DL 1080p target and must appear")
	}
	if contains(allowed, q(quality.SourceWEBDL, 2160)) {
		t.Error("2160p must not appear under a 1080p target")
	}

	// A floored profile hides everything below the floor.
	floored := quality.Profile{
		Target: q(quality.SourceWEBDL, 2160), Floor: ptr(q(quality.SourceWEBDL, 2160)),
	}
	for _, a := range floored.AllowedUnder() {
		if a.Resolution != 2160 {
			t.Errorf("4K profile synthesized %v", a)
		}
	}

	// Books get the book vocabulary and nothing else.
	book := quality.Profile{Target: q(quality.SourceEPUB, 0)}
	for _, a := range book.AllowedUnder() {
		if !quality.IsBookFormat(a.Source) {
			t.Errorf("book profile synthesized %v", a)
		}
	}
}

func contains(list []quality.Quality, want quality.Quality) bool {
	for _, q := range list {
		if q == want {
			return true
		}
	}
	return false
}

func because(why string) string {
	if why == "" {
		return ""
	}
	return " — " + why
}
