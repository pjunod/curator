package domain

import (
	"testing"

	"github.com/pjunod/monarr/internal/domain/hdr"
)

// A season has one HDR tier only when every episode measured the same one
// (ADR 0031 §9); a book has no picture.
func TestCurrentHDRAcrossWantables(t *testing.T) {
	fel := EpisodeWantable{HDR: hdr.TierDVP7FEL}
	p8 := EpisodeWantable{HDR: hdr.TierDVP8}
	cases := []struct {
		name string
		w    Wantable
		want hdr.Tier
	}{
		{"movie", MovieWantable{HDR: hdr.TierHDR10}, hdr.TierHDR10},
		{"episode", fel, hdr.TierDVP7FEL},
		{"season, all alike", SeasonWantable{Episodes: []EpisodeWantable{fel, fel}}, hdr.TierDVP7FEL},
		{"season, mixed", SeasonWantable{Episodes: []EpisodeWantable{fel, p8}}, hdr.TierUnknown},
		{"season, one unmeasured", SeasonWantable{Episodes: []EpisodeWantable{fel, {}}}, hdr.TierUnknown},
		{"season, empty", SeasonWantable{}, hdr.TierUnknown},
		{"book", BookWantable{}, hdr.TierUnknown},
	}
	for _, tc := range cases {
		if got := tc.w.CurrentHDR(); got != tc.want {
			t.Errorf("%s: CurrentHDR = %q, want %q", tc.name, got, tc.want)
		}
	}
}
