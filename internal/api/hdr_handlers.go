package api

import (
	"fmt"
	"net/http"
	"slices"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/domain/hdr"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

// ListHDRTiers implements GET /hdr-tiers: the ADR 0031 vocabulary in the
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

func preflightDTO(p quality.Preflight) *apigen.PreflightPolicy {
	if p == (quality.Preflight{}) {
		return nil
	}
	top, bytes, skip := p.Top, p.Bytes, p.SkipUnreadable
	return &apigen.PreflightPolicy{Top: &top, Bytes: &bytes, SkipUnreadable: &skip}
}

func preflightFromInput(in *apigen.PreflightPolicy) (quality.Preflight, error) {
	var p quality.Preflight
	if in == nil {
		return p, nil
	}
	if in.Top != nil {
		p.Top = *in.Top
	}
	if in.Bytes != nil {
		p.Bytes = *in.Bytes
	}
	if in.SkipUnreadable != nil {
		p.SkipUnreadable = *in.SkipUnreadable
	}
	if p.Top < 0 || p.Top > quality.ProbeAll {
		return p, fmt.Errorf("preflight.top must be between 0 and %d", quality.ProbeAll)
	}
	if p.Bytes < 0 || p.Bytes > quality.MaxProbeBytes {
		return p, fmt.Errorf("preflight.bytes must be between 0 and %d", quality.MaxProbeBytes)
	}
	return p, nil
}

func probeEvidenceDTO(ev *acquisition.ProbeEvidence) *apigen.ProbeEvidence {
	if ev == nil {
		return nil
	}
	out := &apigen.ProbeEvidence{Status: apigen.ProbeEvidenceStatus(ev.Status)}
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	num := func(n int) *int {
		if n == 0 {
			return nil
		}
		return &n
	}
	out.Reason, out.Detail, out.File = str(ev.Reason), str(ev.Detail), str(ev.File)
	out.Container, out.Facts = str(ev.Container), str(ev.Facts)
	out.Articles, out.Missing, out.Bytes = num(ev.Articles), num(ev.Missing), num(ev.Bytes)
	if len(ev.Path) > 0 {
		path := append([]string(nil), ev.Path...)
		out.Path = &path
	}
	if ev.Millis > 0 {
		ms := ev.Millis
		out.Millis = &ms
	}
	if ev.Tier != "" {
		tier := apigen.HDRTier(ev.Tier)
		out.Tier = &tier
	}
	if ev.DoVi != nil {
		out.DolbyVision = dolbyVisionDTO(mediainfo.Info{Video: &mediainfo.VideoInfo{DoVi: ev.DoVi}})
	}
	return out
}

func hdrTiesDTO(in [][]hdr.Tier) *[][]apigen.HDRTier {
	if len(in) == 0 {
		return nil
	}
	out := make([][]apigen.HDRTier, len(in))
	for i, g := range in {
		out[i] = *hdrTiersDTO(g)
	}
	return &out
}

// hdrTiesFromInput validates tie groups: two or more tiers each, every one in
// the order, none in two groups.
func hdrTiesFromInput(in *[][]apigen.HDRTier, order []hdr.Tier) ([][]hdr.Tier, error) {
	if in == nil {
		return nil, nil
	}
	seen := map[hdr.Tier]bool{}
	var out [][]hdr.Tier
	for _, raw := range *in {
		g, err := hdrTiersFromInput("hdrTies", &raw)
		if err != nil {
			return nil, err
		}
		if len(g) < 2 {
			return nil, fmt.Errorf("a tie needs two or more tiers")
		}
		for _, t := range g {
			if !slices.Contains(order, t) {
				return nil, fmt.Errorf("%s is tied but not in the order", t.Label())
			}
			if seen[t] {
				return nil, fmt.Errorf("%s is in two ties", t.Label())
			}
			seen[t] = true
		}
		out = append(out, g)
	}
	return out, nil
}

// ListHDRDevices implements GET /hdr/devices: every plurx connection's
// signed-in playback devices, what each decodes, and the HDR order, ties and
// refusals a profile would take from it (ADR 0031 §5).
func (s *Server) ListHDRDevices(w http.ResponseWriter, r *http.Request) {
	notifiers, err := s.deps.Store.ListNotifiers(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := apigen.HDRDevices{Devices: []apigen.HDRDevice{}, Errors: []struct {
		Message string `json:"message"`
		Source  string `json:"source"`
	}{}}
	for _, cfg := range notifiers {
		if cfg.Type != "plurx" || !cfg.Enabled {
			continue
		}
		source, ok := s.deps.NotifierFactory(cfg).(ports.PlaybackDeviceSource)
		if !ok {
			continue
		}
		devices, err := source.PlaybackDevices(r.Context())
		if err != nil {
			out.Errors = append(out.Errors, struct {
				Message string `json:"message"`
				Source  string `json:"source"`
			}{Message: err.Error(), Source: cfg.Name})
			continue
		}
		for _, d := range devices {
			order, ties, reject := hdr.Derive(d.Caps)
			dto := apigen.HDRDevice{
				Source: cfg.Name, Id: d.ID, User: d.User, Device: d.Device, Client: d.Client,
				LastSeenAt: d.LastSeen, DolbyVision: d.Caps.DolbyVision, Fel: d.Caps.FEL,
				Profile5: d.Caps.Profile5, Hdr10: d.Caps.HDR10, Hlg: d.Caps.HLG, Hdr10plus: d.Caps.HDR10Plus,
				HdrOrder: derefTiers(hdrTiersDTO(order)), HdrReject: derefTiers(hdrTiersDTO(reject)),
				HdrTies: [][]apigen.HDRTier{},
			}
			if t := hdrTiesDTO(ties); t != nil {
				dto.HdrTies = *t
			}
			out.Devices = append(out.Devices, dto)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func derefTiers(t *[]apigen.HDRTier) []apigen.HDRTier {
	if t == nil {
		return []apigen.HDRTier{}
	}
	return *t
}
