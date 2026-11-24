// Package hdr is the one vocabulary for "what kind of HDR is this" (ADR
// 0026). A release name's claim, a probe's measurement and a target
// profile's preference order all reduce to a Tier, so they can be compared
// with each other without any of them knowing about the others.
package hdr

import "strings"

// Tier names an HDR presentation as far as picture and playback differ.
type Tier string

// Tiers, most specific first. The empty Tier means nothing is known.
const (
	TierUnknown   Tier = ""
	TierDVP7FEL   Tier = "dv-p7-fel" // dual layer, full enhancement layer
	TierDVP7MEL   Tier = "dv-p7-mel" // dual layer, minimal enhancement layer
	TierDVP7      Tier = "dv-p7"     // dual layer, EL type not known
	TierDVP8      Tier = "dv-p8"     // single layer with an HDR10/SDR/HLG base
	TierDVP5      Tier = "dv-p5"     // single layer, no compatible base
	TierDV        Tier = "dv"        // Dolby Vision, profile not known
	TierHDR10Plus Tier = "hdr10plus"
	TierHDR10     Tier = "hdr10"
	TierHLG       Tier = "hlg"
	TierSDR       Tier = "sdr"
)

// Tiers lists every named tier in the default preference order: the order a
// target profile starts from when someone first asks for an HDR order.
var Tiers = []Tier{
	TierDVP7FEL, TierDVP7MEL, TierDVP8, TierHDR10Plus, TierHDR10, TierHLG, TierDVP5, TierSDR,
}

// Valid reports whether t is a tier a profile may name.
func (t Tier) Valid() bool {
	for _, x := range Tiers {
		if x == t {
			return true
		}
	}
	return false
}

// DolbyVision reports whether the tier is any Dolby Vision tier.
func (t Tier) DolbyVision() bool { return strings.HasPrefix(string(t), "dv") }

// Possible lists the named tiers a partly-known tier could turn out to be.
// A profile ranks a partly-known tier as the worst of them: not knowing
// whether a profile 7 release is FEL must never rank it with FEL (the ADR
// 0013 §5 rule that a guess never outranks a measurement).
func (t Tier) Possible() []Tier {
	switch t {
	case TierDVP7:
		return []Tier{TierDVP7FEL, TierDVP7MEL}
	case TierDV:
		return []Tier{TierDVP7FEL, TierDVP7MEL, TierDVP8, TierDVP5}
	case TierUnknown:
		return nil
	}
	return []Tier{t}
}

// Label renders a tier for people.
func (t Tier) Label() string {
	switch t {
	case TierDVP7FEL:
		return "DV P7 FEL"
	case TierDVP7MEL:
		return "DV P7 MEL"
	case TierDVP7:
		return "DV P7"
	case TierDVP8:
		return "DV P8"
	case TierDVP5:
		return "DV P5"
	case TierDV:
		return "Dolby Vision"
	case TierHDR10Plus:
		return "HDR10+"
	case TierHDR10:
		return "HDR10"
	case TierHLG:
		return "HLG"
	case TierSDR:
		return "SDR"
	}
	return "unknown"
}

// Formats (the strings mediainfo records in VideoInfo.HDR).
const (
	FormatDV        = "dv"
	FormatHDR10     = "hdr10"
	FormatHDR10Plus = "hdr10plus"
	FormatHLG       = "hlg"
)

// Enhancement-layer types, as mediainfo/dovi names them.
const (
	ELNone = "none"
	ELMEL  = "mel"
	ELFEL  = "fel"
)

// Classify reduces formats and Dolby Vision facts to a tier. It serves both
// a measurement (measured=true: no HDR format means SDR, because the probe
// would have seen one) and a name claim (measured=false: a name that says
// nothing about HDR says nothing).
func Classify(formats []string, dvProfile int, elType string, measured bool) Tier {
	has := func(f string) bool {
		for _, x := range formats {
			if x == f {
				return true
			}
		}
		return false
	}
	switch {
	case has(FormatDV) || dvProfile > 0:
		switch dvProfile {
		case 7:
			switch elType {
			case ELFEL:
				return TierDVP7FEL
			case ELMEL:
				return TierDVP7MEL
			}
			return TierDVP7
		case 8:
			return TierDVP8
		case 5:
			return TierDVP5
		}
		return TierDV
	case has(FormatHDR10Plus):
		return TierHDR10Plus
	case has(FormatHDR10):
		return TierHDR10
	case has(FormatHLG):
		return TierHLG
	}
	if measured {
		return TierSDR
	}
	return TierUnknown
}

// Confidence of a claim inferred from a name.
const (
	ConfidenceHigh   = "high"   // the name states it outright ("DV.P7.FEL")
	ConfidenceMedium = "medium" // inferred from source and companion tags
	ConfidenceLow    = "low"    // "DV" and nothing to place it
)

// Claim is what a release name says about HDR.
type Claim struct {
	Formats   []string // FormatDV, FormatHDR10, FormatHDR10Plus, FormatHLG
	SDR       bool     // the name says SDR outright
	DVProfile int      // 0 unknown
	ELType    string   // "" unless the name says FEL or MEL
	// Confidence qualifies DVProfile when it was inferred rather than stated;
	// empty when the name says nothing about Dolby Vision.
	Confidence string
}

// Tier reduces the claim.
func (c Claim) Tier() Tier {
	if c.SDR && len(c.Formats) == 0 {
		return TierSDR
	}
	return Classify(c.Formats, c.DVProfile, c.ELType, false)
}

// Evidence is a tier together with how it is known, which is what ranking
// needs: a measurement and a name stating its profile outright are facts; an
// inference from companion tags is a guess.
type Evidence struct {
	Tier       Tier
	Measured   bool
	Confidence string // for a claim: ConfidenceHigh/Medium/Low
}

// Sure reports whether the evidence is strong enough to refuse a release on.
func (e Evidence) Sure() bool { return e.Measured || e.Confidence == ConfidenceHigh }

// Claimed wraps a name claim as evidence. A claim with no Dolby Vision profile
// to qualify (HDR10, HDR10+, HLG, SDR stated outright) is stated, not
// inferred, so it counts as high confidence.
func Claimed(c Claim) Evidence {
	e := Evidence{Tier: c.Tier(), Confidence: c.Confidence}
	if e.Tier != TierUnknown && !e.Tier.DolbyVision() {
		e.Confidence = ConfidenceHigh
	}
	return e
}

// Measure wraps a measured tier as evidence.
func Measure(t Tier) Evidence { return Evidence{Tier: t, Measured: true} }

// Better reports whether a ranks strictly above b, with measurement breaking
// a tie of equal tiers: a measured DV P7 FEL is worth more than a name that
// merely says so.
func Better(rank func(Tier) int, a, b Evidence) bool {
	ra, rb := rank(a.Tier), rank(b.Tier)
	if ra != rb {
		return ra < rb
	}
	return a.Measured && !b.Measured
}
