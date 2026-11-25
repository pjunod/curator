package hdr

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		formats  []string
		profile  int
		el       string
		measured bool
		want     Tier
	}{
		{"P7 FEL", []string{FormatDV, FormatHDR10}, 7, ELFEL, true, TierDVP7FEL},
		{"P7 MEL", []string{FormatDV}, 7, ELMEL, true, TierDVP7MEL},
		{"P7 EL unknown", []string{FormatDV}, 7, "", true, TierDVP7},
		{"P8", []string{FormatDV, FormatHDR10}, 8, ELNone, true, TierDVP8},
		{"P5", []string{FormatDV}, 5, ELNone, true, TierDVP5},
		{"DV without a record", []string{FormatDV}, 0, "", true, TierDV},
		{"HDR10+ wins over HDR10", []string{FormatHDR10, FormatHDR10Plus}, 0, "", true, TierHDR10Plus},
		{"HDR10", []string{FormatHDR10}, 0, "", true, TierHDR10},
		{"HLG", []string{FormatHLG}, 0, "", true, TierHLG},
		{"measured nothing is SDR", nil, 0, "", true, TierSDR},
		{"a silent name is unknown", nil, 0, "", false, TierUnknown},
	}
	for _, tc := range cases {
		if got := Classify(tc.formats, tc.profile, tc.el, tc.measured); got != tc.want {
			t.Errorf("%s: Classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPossibleNeverOverclaims(t *testing.T) {
	if got := TierDVP7.Possible(); !reflect.DeepEqual(got, []Tier{TierDVP7FEL, TierDVP7MEL}) {
		t.Errorf("dv-p7 possible = %v", got)
	}
	if got := TierHDR10.Possible(); !reflect.DeepEqual(got, []Tier{TierHDR10}) {
		t.Errorf("hdr10 possible = %v", got)
	}
	if TierUnknown.Possible() != nil {
		t.Error("unknown tier claims possibilities")
	}
	for _, tier := range Tiers {
		if !tier.Valid() {
			t.Errorf("%q in Tiers but not Valid", tier)
		}
	}
	if TierDVP7.Valid() || TierDV.Valid() {
		t.Error("a partly-known tier must not be nameable in a profile order")
	}
}

func TestClaimTier(t *testing.T) {
	if got := (Claim{SDR: true}).Tier(); got != TierSDR {
		t.Errorf("SDR claim = %q", got)
	}
	if got := (Claim{SDR: true, Formats: []string{FormatHDR10}}).Tier(); got != TierHDR10 {
		t.Errorf("contradictory SDR+HDR10 claim = %q, want the HDR tag to win", got)
	}
	if got := (Claim{}).Tier(); got != TierUnknown {
		t.Errorf("empty claim = %q", got)
	}
}
