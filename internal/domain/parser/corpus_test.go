package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/monarr-media/monarr/internal/domain/quality"
)

// corpusCase mirrors testdata/releases/*.json. Zero-valued expectation
// fields mean "don't care" EXCEPT title, which is always asserted.
type corpusCase struct {
	Title  string `json:"title"`
	Expect struct {
		Title      string `json:"title"`
		Year       int    `json:"year"`
		Season     *int   `json:"season"`
		Episodes   []int  `json:"episodes"`
		SeasonPack bool   `json:"seasonPack"`
		Daily      string `json:"daily"`
		Source     string `json:"source"`
		Resolution int    `json:"resolution"`
		Proper     bool   `json:"proper"`
		Repack     bool   `json:"repack"`
		Group      string `json:"group"`
	} `json:"expect"`
}

// TestGoldenCorpus runs every case in testdata/releases and reports a
// conformance percentage. The build fails if any case fails — the corpus IS
// the parser's spec.
func TestGoldenCorpus(t *testing.T) {
	root := filepath.Join("..", "..", "..", "testdata", "releases")
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus files found in %s (err=%v)", root, err)
	}

	total, passed := 0, 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var cases []corpusCase
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, c := range cases {
			total++
			got := Parse(c.Title)
			var errs []string
			exp := c.Expect
			if got.Title != exp.Title {
				errs = append(errs, fmt.Sprintf("title %q != %q", got.Title, exp.Title))
			}
			if exp.Year != 0 && got.Year != exp.Year {
				errs = append(errs, fmt.Sprintf("year %d != %d", got.Year, exp.Year))
			}
			if exp.Season != nil && got.Season != *exp.Season {
				errs = append(errs, fmt.Sprintf("season %d != %d", got.Season, *exp.Season))
			}
			if exp.Episodes != nil && !reflect.DeepEqual(got.Episodes, exp.Episodes) {
				errs = append(errs, fmt.Sprintf("episodes %v != %v", got.Episodes, exp.Episodes))
			}
			if got.SeasonPack != exp.SeasonPack {
				errs = append(errs, fmt.Sprintf("seasonPack %v != %v", got.SeasonPack, exp.SeasonPack))
			}
			if exp.Daily != "" && got.Daily != exp.Daily {
				errs = append(errs, fmt.Sprintf("daily %q != %q", got.Daily, exp.Daily))
			}
			if exp.Source != "" && string(got.Quality.Source) != exp.Source {
				errs = append(errs, fmt.Sprintf("source %q != %q", got.Quality.Source, exp.Source))
			}
			if exp.Resolution != 0 && got.Quality.Resolution != exp.Resolution {
				errs = append(errs, fmt.Sprintf("resolution %d != %d", got.Quality.Resolution, exp.Resolution))
			}
			if got.Proper != exp.Proper {
				errs = append(errs, fmt.Sprintf("proper %v != %v", got.Proper, exp.Proper))
			}
			if got.Repack != exp.Repack {
				errs = append(errs, fmt.Sprintf("repack %v != %v", got.Repack, exp.Repack))
			}
			if exp.Group != "" && got.Group != exp.Group {
				errs = append(errs, fmt.Sprintf("group %q != %q", got.Group, exp.Group))
			}
			if len(errs) == 0 {
				passed++
			} else {
				t.Errorf("%s: %q → %v", filepath.Base(file), c.Title, errs)
			}
		}
	}
	pct := 100 * float64(passed) / float64(total)
	t.Logf("parser conformance: %d/%d (%.1f%%)", passed, total, pct)
	if passed != total {
		t.Errorf("conformance %.1f%% — corpus must pass 100%%", pct)
	}
}

func TestProfileBasics(t *testing.T) {
	profiles := quality.DefaultProfiles()
	hd := profiles[1]
	if !hd.IsAllowed(quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}) {
		t.Error("HD-1080p should allow WEBDL-1080")
	}
	if hd.IsAllowed(quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160}) {
		t.Error("HD-1080p should not allow 2160p")
	}
	// Unknown source matches on resolution (scanned files).
	if !hd.IsAllowed(quality.Quality{Source: quality.SourceUnknown, Resolution: 1080}) {
		t.Error("unknown-source 1080 should be allowed by resolution")
	}
	if !hd.MeetsCutoff(quality.Quality{Source: quality.SourceBluray, Resolution: 1080}) {
		t.Error("bluray-1080 should meet WEBDL-1080 cutoff")
	}
	if hd.MeetsCutoff(quality.Quality{Source: quality.SourceHDTV, Resolution: 1080}) {
		t.Error("hdtv-1080 should be below WEBDL-1080 cutoff")
	}
	if !quality.Better(quality.Quality{Source: quality.SourceHDTV, Resolution: 1080}, quality.Quality{Source: quality.SourceBluray, Resolution: 720}) {
		t.Error("resolution should dominate source in ranking")
	}
	rt := quality.FromString(quality.Quality{Source: quality.SourceRemux, Resolution: 2160}.String())
	if rt.Source != quality.SourceRemux || rt.Resolution != 2160 {
		t.Errorf("string round-trip failed: %+v", rt)
	}
}

func FuzzParse(f *testing.F) {
	f.Add("Show.Name.S01E02.1080p.WEB-DL.x264-GROUP")
	f.Add("Movie (2024) [1080p] REMUX")
	f.Add("[SubsPlease] Anime - 05 (1080p)")
	f.Add("")
	f.Add("....----____")
	f.Fuzz(func(t *testing.T, s string) {
		_ = Parse(s) // must never panic
	})
}
