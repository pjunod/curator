package hdr

// DeviceCaps is what a playback device decodes (ADR 0031 §5), as Cinema
// reports it.
type DeviceCaps struct {
	Name string
	// DolbyVision: the device renders Dolby Vision at all (profile 8, and
	// profile 7's base layer with its RPU).
	DolbyVision bool
	// FEL: it also decodes a profile 7 full enhancement layer. Without it a
	// FEL stream plays exactly like its MEL or 8.1 counterpart.
	FEL bool
	// Profile5: it plays profile 5 correctly (no HDR10 base to fall back to).
	Profile5 bool
	// HDR10Plus: it renders HDR10+ dynamic metadata rather than the static
	// HDR10 base under it.
	HDR10Plus bool
	HDR10     bool
	HLG       bool
}

// rendering is how a device presents a tier, best first. Two tiers that a
// device renders the same way are the same picture there.
type rendering int

const (
	renderFEL rendering = iota
	renderDV
	renderHDR10Plus
	renderHDR10
	renderHLG
	renderSDR
	renderBroken // plays with the wrong colours, or not at all
)

func (c DeviceCaps) render(t Tier) rendering {
	switch t {
	case TierDVP7FEL:
		if c.DolbyVision && c.FEL {
			return renderFEL
		}
		return c.dvOrBase()
	case TierDVP7MEL, TierDVP8:
		return c.dvOrBase()
	case TierDVP5:
		if c.DolbyVision && c.Profile5 {
			return renderDV
		}
		return renderBroken // no HDR10 base: the picture is wrong without DV
	case TierHDR10Plus:
		if c.HDR10Plus {
			return renderHDR10Plus
		}
		return c.hdr10OrSDR()
	case TierHDR10:
		return c.hdr10OrSDR()
	case TierHLG:
		if c.HLG {
			return renderHLG
		}
		return renderSDR
	}
	return renderSDR
}

// dvOrBase: a dual- or single-layer stream with an HDR10 base plays its base
// where Dolby Vision is not decoded.
func (c DeviceCaps) dvOrBase() rendering {
	if c.DolbyVision {
		return renderDV
	}
	return c.hdr10OrSDR()
}

func (c DeviceCaps) hdr10OrSDR() rendering {
	if c.HDR10 {
		return renderHDR10
	}
	return renderSDR // tone-mapped down; still a picture
}

// Derive turns what a device decodes into a profile's HDR order, ties and
// refusals: tiers ordered by how the device renders them, tiers it renders
// identically tied (so Curator neither pays for an upgrade the device cannot
// show nor replaces a file with an equal one), and tiers it renders wrongly
// refused. SDR is left out of the order, as the suggested order does.
func Derive(c DeviceCaps) (order []Tier, ties [][]Tier, reject []Tier) {
	byRendering := map[rendering][]Tier{}
	for _, t := range Tiers {
		if t == TierSDR {
			continue
		}
		r := c.render(t)
		if r == renderBroken {
			reject = append(reject, t)
			continue
		}
		byRendering[r] = append(byRendering[r], t)
	}
	for r := renderFEL; r < renderBroken; r++ {
		group := byRendering[r]
		order = append(order, group...)
		if len(group) > 1 {
			ties = append(ties, group)
		}
	}
	return order, ties, reject
}
