package api

import (
	"fmt"
	"net/http"
	"slices"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// ListHDRTiers implements GET /hdr-tiers: the ADR 0026 vocabulary in the
// default preference order.
func (s *Server) ListHDRTiers(w http.ResponseWriter, _ *http.Request) {
	out := make([]apigen.HDRTierOption, 0, len(hdr.Tiers))
	for _, t := range hdr.Tiers {
		out = append(out, apigen.HDRTierOption{Tier: apigen.HDRTier(t), Label: t.Label()})
	}
	writeJSON(w, http.StatusOK, out)
}

func hdrTiersDTO(in []hdr.Tier) *[]apigen.HDRTier {
	if len(in) == 0 {
		return nil
	}
	out := make([]apigen.HDRTier, len(in))
	for i, t := range in {
		out[i] = apigen.HDRTier(t)
	}
	return &out
}

// hdrTiersFromInput validates a profile's HDR order or refusal list: every
// tier from GET /hdr-tiers, none twice.
func hdrTiersFromInput(field string, in *[]apigen.HDRTier) ([]hdr.Tier, error) {
	if in == nil {
		return nil, nil
	}
	var out []hdr.Tier
	for _, raw := range *in {
		t := hdr.Tier(raw)
		if !t.Valid() {
			return nil, fmt.Errorf("%q is not an HDR tier a profile can name in %s; see GET /hdr-tiers", raw, field)
		}
		if slices.Contains(out, t) {
			return nil, fmt.Errorf("%s names %s twice", field, t.Label())
		}
		out = append(out, t)
	}
	return out, nil
}

func releaseHDRDTO(c acquisition.CandidateHDR) apigen.ReleaseHDR {
	out := apigen.ReleaseHDR{Tier: apigen.HDRTier(c.Tier), Label: c.Label, Measured: c.Measured}
	if c.DVProfile != 0 {
		p := c.DVProfile
		out.DvProfile = &p
	}
	if c.ELType != "" {
		el := apigen.ReleaseHDRElType(c.ELType)
		out.ElType = &el
	}
	if c.Confidence != "" {
		conf := apigen.ReleaseHDRConfidence(c.Confidence)
		out.Confidence = &conf
	}
	return out
}

// dolbyVisionDTO renders a file's measured Dolby Vision record; nil when the
// file carries none the probe could read.
func dolbyVisionDTO(info mediainfo.Info) *apigen.DolbyVision {
	if info.Video == nil || info.Video.DoVi == nil {
		return nil
	}
	d := info.Video.DoVi
	out := &apigen.DolbyVision{
		Profile: d.Profile, Rpu: d.RPU, El: d.EL, Bl: d.BL, BlCompatId: d.BLCompatID,
		Source: apigen.DolbyVisionSource(d.Source), Label: d.Label(),
	}
	optInt := func(n int) *int {
		if n == 0 {
			return nil
		}
		return &n
	}
	out.Level, out.MaxCll, out.MaxFall, out.MasteringMaxNits = optInt(d.Level), optInt(d.MaxCLL), optInt(d.MaxFALL), optInt(d.MasteringMaxNits)
	if d.ELType != "" {
		el := apigen.DolbyVisionElType(d.ELType)
		out.ElType = &el
	}
	if d.CMVersion != "" {
		cm := apigen.DolbyVisionCmVersion(d.CMVersion)
		out.CmVersion = &cm
	}
	return out
}
