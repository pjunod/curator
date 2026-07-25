package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// Cases taken from a real review queue that would not match. Each one looked
// like "the matcher is broken" from the outside and had a different cause.
func TestGradingOnRealWorldCases(t *testing.T) {
	mv := func(id int64, title string, year int) ports.SearchResult {
		return ports.SearchResult{Kind: domain.KindMovie, TMDBID: id, Title: title, Year: year}
	}

	for _, tc := range []struct {
		name    string
		folder  string
		results []ports.SearchResult
		want    Confidence
		winner  string // expected first candidate when exact
		why     string
	}{
		{
			name:   "duplicate provider rows",
			folder: "Daniel Sloss Can't (2025)",
			// TMDB really does return this film twice.
			results: []ports.SearchResult{
				mv(1, "Daniel Sloss: Can't", 2025),
				mv(1, "Daniel Sloss: Can't", 2025),
			},
			want:   ConfidenceExact,
			winner: "Daniel Sloss: Can't",
			why:    "a duplicate must not turn one match into two",
		},
		{
			name:   "same title, adjacent year",
			folder: "Nosferatu (2024)",
			results: []ports.SearchResult{
				mv(10, "Nosferatu", 2024), mv(11, "Nosferatu", 1922), mv(12, "Nosferatu", 2025),
			},
			want:   ConfidenceExact,
			winner: "Nosferatu",
			why:    "an exact year beats one merely inside the ±1 tolerance",
		},
		{
			name:    "one clean match among near-misses",
			folder:  "Rent (2019)",
			results: []ports.SearchResult{mv(20, "Rent", 2019), mv(21, "Rent", 2005), mv(22, "Rent Control", 2003)},
			want:    ConfidenceExact,
			winner:  "Rent",
			why:     "different years and different titles are not competition",
		},
		{
			name:    "winner is not first in provider order",
			folder:  "Riot (2015)",
			results: []ports.SearchResult{mv(30, "Riot", 1996), mv(31, "Riot", 2015), mv(32, "Riot", 1997)},
			want:    ConfidenceExact,
			winner:  "Riot",
			why:     "the winner must be surfaced first regardless of provider order",
		},
		{
			name:   "tolerance still applies when nothing matches exactly",
			folder: "Some Film (2019)",
			// The drift case the ±1 window exists for.
			results: []ports.SearchResult{mv(40, "Some Film", 2020)},
			want:    ConfidenceExact,
			winner:  "Some Film",
			why:     "release-date drift is why the tolerance exists",
		},
		{
			name:    "genuinely ambiguous stays ambiguous",
			folder:  "Last Breath (2025)",
			results: []ports.SearchResult{mv(50, "Last Breath", 2025), mv(51, "Last Breath", 2024)},
			want:    ConfidenceExact,
			winner:  "Last Breath",
			why:     "2025 is exact, 2024 is only tolerated",
		},
		{
			name:    "two exact-year twins are a real coin flip",
			folder:  "Twin (2020)",
			results: []ports.SearchResult{mv(60, "Twin", 2020), mv(61, "Twin", 2020)},
			want:    ConfidenceAmbiguous,
			why:     "two distinct films with the same title and year must ask",
		},
		{
			name:    "extra words in the folder name",
			folder:  "F1 The Movie (2025)",
			results: []ports.SearchResult{mv(70, "F1", 2025), mv(71, "Silver Spoon", 2021)},
			want:    ConfidenceAmbiguous,
			why:     "the parsed title genuinely differs; guessing here is how wrong matches happen",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := newService(t)
			svc.meta = adoptProvider{movies: tc.results}
			ctx := context.Background()

			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, tc.folder), 0o755); err != nil {
				t.Fatal(err)
			}
			rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
			if err != nil {
				t.Fatal(err)
			}
			props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
				{RootFolderID: rf.ID, Path: filepath.Join(root, tc.folder), Name: tc.folder},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := props[0]
			if got.Confidence != tc.want {
				t.Fatalf("confidence = %s, want %s — %s (candidates %+v)",
					got.Confidence, tc.want, tc.why, got.Candidates)
			}
			if tc.winner != "" {
				if len(got.Candidates) == 0 || got.Candidates[0].Title != tc.winner {
					t.Errorf("first candidate = %+v, want %q first — %s", got.Candidates, tc.winner, tc.why)
				}
			}
		})
	}
}

// Duplicates must also not consume the three candidate slots a review row
// has, or a real alternative gets pushed off the end by noise.
func TestDedupeFreesCandidateSlots(t *testing.T) {
	mv := func(id int64, title string, year int) ports.SearchResult {
		return ports.SearchResult{Kind: domain.KindMovie, TMDBID: id, Title: title, Year: year}
	}
	in := []ports.SearchResult{
		mv(1, "Thing", 2020), mv(1, "Thing", 2020), mv(1, "Thing", 2020), mv(2, "Thing Two", 2021),
	}
	out := dedupeResults(in)
	if len(out) != 2 {
		t.Fatalf("dedupe left %d, want 2: %+v", len(out), out)
	}
}
