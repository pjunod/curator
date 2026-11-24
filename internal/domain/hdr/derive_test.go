package hdr

import (
	"reflect"
	"testing"
)

func TestDerive(t *testing.T) {
	cases := []struct {
		name   string
		caps   DeviceCaps
		order  []Tier
		ties   [][]Tier
		reject []Tier
	}{
		{
			// Decodes everything: the full order, nothing tied, nothing refused.
			name:  "full FEL player",
			caps:  DeviceCaps{DolbyVision: true, FEL: true, Profile5: true, HDR10Plus: true, HDR10: true, HLG: true},
			order: []Tier{TierDVP7FEL, TierDVP7MEL, TierDVP8, TierDVP5, TierHDR10Plus, TierHDR10, TierHLG},
			ties:  [][]Tier{{TierDVP7MEL, TierDVP8, TierDVP5}},
		},
		{
			// Drops the EL: FEL, MEL and 8.1 are one picture; HDR10+ falls
			// back to its static base.
			name:  "DV TV without FEL or HDR10+",
			caps:  DeviceCaps{DolbyVision: true, Profile5: true, HDR10: true, HLG: true},
			order: []Tier{TierDVP7FEL, TierDVP7MEL, TierDVP8, TierDVP5, TierHDR10Plus, TierHDR10, TierHLG},
			ties:  [][]Tier{{TierDVP7FEL, TierDVP7MEL, TierDVP8, TierDVP5}, {TierHDR10Plus, TierHDR10}},
		},
		{
			// A browser with HDR10 only: every Dolby Vision tier with a base
			// plays that base, and profile 5, which has none, is refused.
			name:   "HDR10 browser",
			caps:   DeviceCaps{HDR10: true},
			order:  []Tier{TierDVP7FEL, TierDVP7MEL, TierDVP8, TierHDR10Plus, TierHDR10, TierHLG},
			ties:   [][]Tier{{TierDVP7FEL, TierDVP7MEL, TierDVP8, TierHDR10Plus, TierHDR10}},
			reject: []Tier{TierDVP5},
		},
	}
	for _, tc := range cases {
		order, ties, reject := Derive(tc.caps)
		if !reflect.DeepEqual(order, tc.order) || !reflect.DeepEqual(ties, tc.ties) || !reflect.DeepEqual(reject, tc.reject) {
			t.Errorf("%s:\n order  %v\n ties   %v\n reject %v\nwant\n order  %v\n ties   %v\n reject %v",
				tc.name, order, ties, reject, tc.order, tc.ties, tc.reject)
		}
	}
}
