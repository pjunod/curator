package quality_test

import (
	"testing"

	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/quality"
)

// The rendered "UHD Reference" profile (ADR 0031 §4): FEL › MEL › 8.1 ›
// HDR10+ › HDR10, never profile 5.
func uhdReference() quality.Profile {
	return quality.Profile{
		Name: "UHD Reference", Target: q(quality.SourceRemux, 2160), UpgradesAllowed: true,
		HDROrder:  []hdr.Tier{hdr.TierDVP7FEL, hdr.TierDVP7MEL, hdr.TierDVP8, hdr.TierHDR10Plus, hdr.TierHDR10},
		HDRReject: []hdr.Tier{hdr.TierDVP5},
	}
}

func TestHDRRank(t *testing.T) {
	p := uhdReference()
	cases := []struct {
		tier hdr.Tier
		want int
	}{
		{hdr.TierDVP7FEL, 0},
		{hdr.TierDVP7MEL, 1},
		// Not knowing FEL from MEL ranks as the worse of the two.
		{hdr.TierDVP7, 1},
		{hdr.TierDVP8, 2},
		{hdr.TierHDR10, 4},
		// Unlisted and unknown rank below everything listed.
		{hdr.TierSDR, 5},
		{hdr.TierUnknown, 5},
		// "Dolby Vision, profile unknown" could be profile 5: worst case.
		{hdr.TierDV, 5},
	}
	for _, tc := range cases {
		if got := p.HDRRank(tc.tier); got != tc.want {
			t.Errorf("HDRRank(%q) = %d, want %d", tc.tier, got, tc.want)
		}
	}
	if got := (quality.Profile{}).HDRRank(hdr.TierSDR); got != 0 {
		t.Errorf("a profile with no order ranks tiers apart: %d", got)
	}
}

func TestHDRAcceptableRefusesOnlyOnSureEvidence(t *testing.T) {
	p := uhdReference()
	cases := []struct {
		name string
		e    hdr.Evidence
		want bool
	}{
		{"measured P5", hdr.Measure(hdr.TierDVP5), false},
		{"named P5 outright", hdr.Evidence{Tier: hdr.TierDVP5, Confidence: hdr.ConfidenceHigh}, false},
		// A web release tagged DV without HDR is probably P5. Probably is not
		// enough to refuse: the pre-flight probe or the import will tell.
		{"inferred P5", hdr.Evidence{Tier: hdr.TierDVP5, Confidence: hdr.ConfidenceMedium}, true},
		{"measured FEL", hdr.Measure(hdr.TierDVP7FEL), true},
		{"unknown", hdr.Evidence{}, true},
		{"DV, profile unknown", hdr.Evidence{Tier: hdr.TierDV, Confidence: hdr.ConfidenceLow}, true},
	}
	for _, tc := range cases {
		if got := p.HDRAcceptable(tc.e); got != tc.want {
			t.Errorf("%s: HDRAcceptable = %v, want %v", tc.name, got, tc.want)
		}
	}
	both := quality.Profile{HDRReject: []hdr.Tier{hdr.TierDVP7FEL, hdr.TierDVP7MEL}}
	if both.HDRAcceptable(hdr.Evidence{Tier: hdr.TierDVP7, Confidence: hdr.ConfidenceMedium}) {
		t.Error("a P7 claim was accepted though every P7 tier is refused")
	}
}

func TestHDRMetIsTheDontChurnRule(t *testing.T) {
	p := uhdReference()
	if !p.HDRMet(hdr.TierUnknown) {
		t.Error("an unmeasured file made the item wanted on the HDR axis")
	}
	if !p.HDRMet(hdr.TierDVP7FEL) {
		t.Error("the top tier is not met")
	}
	if p.HDRMet(hdr.TierDVP8) {
		t.Error("8.1 under a FEL-first order is met")
	}
	if !(quality.Profile{}).HDRMet(hdr.TierSDR) {
		t.Error("a profile with no HDR order hunts HDR")
	}
	at := q(quality.SourceRemux, 2160)
	if p.Satisfied(at, true, quality.Audio{}, hdr.TierDVP8) {
		t.Error("a remux at the target with 8.1 is satisfied under a FEL-first order")
	}
}

func TestUpgradeOnTheHDRAxis(t *testing.T) {
	p := uhdReference()
	remux := q(quality.SourceRemux, 2160)
	webdl := q(quality.SourceWEBDL, 2160)
	cases := []struct {
		name    string
		release quality.Quality
		relHDR  hdr.Evidence
		current quality.Quality
		curHDR  hdr.Tier
		want    bool
	}{
		{"measured FEL remux over an 8.1 remux", remux, hdr.Measure(hdr.TierDVP7FEL), remux, hdr.TierDVP8, true},
		// MEL ranks above 8.1 in this order, and an unknown-EL P7 ranks as MEL.
		{"named P7 remux over an 8.1 remux", remux, hdr.Evidence{Tier: hdr.TierDVP7, Confidence: hdr.ConfidenceMedium}, remux, hdr.TierDVP8, true},
		{"8.1 over FEL", remux, hdr.Measure(hdr.TierDVP8), remux, hdr.TierDVP7FEL, false},
		// HDR never trades quality away.
		{"FEL web over an 8.1 remux", webdl, hdr.Measure(hdr.TierDVP7FEL), remux, hdr.TierDVP8, false},
		{"silent name over 8.1", remux, hdr.Evidence{}, remux, hdr.TierDVP8, false},
		{"anything over an unmeasured file", remux, hdr.Measure(hdr.TierDVP7FEL), remux, hdr.TierUnknown, false},
		{"measured P5 is refused", remux, hdr.Measure(hdr.TierDVP5), remux, hdr.TierHDR10, false},
	}
	for _, tc := range cases {
		got := p.Upgrade(tc.release, nil, tc.relHDR, tc.current, true, quality.Audio{}, tc.curHDR)
		if got != tc.want {
			t.Errorf("%s: Upgrade = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Without an HDR order the ADR 0014 rule is untouched.
	plain := quality.Profile{Target: remux, UpgradesAllowed: true}
	if plain.Upgrade(remux, nil, hdr.Measure(hdr.TierDVP7FEL), remux, true, quality.Audio{}, hdr.TierDVP8) {
		t.Error("a profile with no HDR order upgraded on HDR")
	}
}

func TestSentenceNamesTheHDROrder(t *testing.T) {
	got := uhdReference().Sentence()
	want := "hunts the best release up to Remux 2160p, then stops; prefers DV P7 FEL › DV P7 MEL › DV P8 › HDR10+ › HDR10; never DV P5"
	if got != want {
		t.Errorf("Sentence =\n %q\nwant\n %q", got, want)
	}
}

// Ties (ADR 0031 §5): on a device that drops the enhancement layer, FEL, MEL
// and 8.1 are one picture. They rank as one, so FEL is not hunted over 8.1
// and an FEL file is not replaced by 8.1 either.
func TestHDRTiesRankAsOne(t *testing.T) {
	p := uhdReference()
	p.HDRTies = [][]hdr.Tier{{hdr.TierDVP7FEL, hdr.TierDVP7MEL, hdr.TierDVP8}}
	for _, tier := range []hdr.Tier{hdr.TierDVP7FEL, hdr.TierDVP7MEL, hdr.TierDVP8, hdr.TierDVP7} {
		if got := p.HDRRank(tier); got != 0 {
			t.Errorf("HDRRank(%q) = %d, want 0 inside the tie", tier, got)
		}
	}
	if p.HDRRank(hdr.TierHDR10) != 4 {
		t.Errorf("HDR10 rank = %d, want 4", p.HDRRank(hdr.TierHDR10))
	}
	if !p.HDRMet(hdr.TierDVP8) {
		t.Error("8.1 is not met though it ties with the top tier")
	}
	remux := q(quality.SourceRemux, 2160)
	if p.Upgrade(remux, nil, hdr.Measure(hdr.TierDVP7FEL), remux, true, quality.Audio{}, hdr.TierDVP8) {
		t.Error("FEL counted as an upgrade over a tied 8.1")
	}
	want := "hunts the best release up to Remux 2160p, then stops; prefers DV P7 FEL = DV P7 MEL = DV P8 › HDR10+ › HDR10; never DV P5"
	if got := p.Sentence(); got != want {
		t.Errorf("Sentence =\n %q\nwant\n %q", got, want)
	}
}
