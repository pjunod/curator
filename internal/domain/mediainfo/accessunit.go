package mediainfo

import "github.com/pjunod/monarr/internal/domain/mediainfo/dovi"

// Bounds on reading one access unit. An RPU is a few hundred bytes and an SEI
// a few KiB; a NAL that claims more than maxNALBody is a slice, which is
// skipped by seeking (free) rather than read (budgeted).
const (
	maxNALBody = 64 << 10
	maxAUNALs  = 512
)

// frameFacts is what the first video frame says that no header does.
type frameFacts struct {
	rpu       *dovi.RPU
	hdr10Plus bool
}

// scanAccessUnit walks the length-prefixed HEVC NAL units of one sample in
// [start, end) — the layout both Matroska and MP4 store — and decodes the
// first Dolby Vision RPU and any HDR10+ SEI. Only NAL headers and those two
// small payloads are read; slices are stepped over, so even a multi-MiB UHD
// keyframe costs a few KiB of budget.
//
// It is best-effort by design: a sample that runs past the bytes available (a
// pre-flight probe holding only the head of a file) or past the budget simply
// yields fewer facts, never an error.
func scanAccessUnit(c *cursor, start, end int64, lengthSize int) frameFacts {
	var f frameFacts
	if lengthSize < 1 || lengthSize > 4 || start < 0 || end > c.size {
		return f
	}
	prefix := int64(lengthSize)
	pos := start
	for n := 0; n < maxAUNALs && pos+prefix+2 <= end; n++ {
		if err := c.seek(pos); err != nil {
			return f
		}
		hdr, err := c.read(prefix + 2)
		if err != nil {
			return f
		}
		var length int64
		for _, x := range hdr[:lengthSize] {
			length = length<<8 | int64(x)
		}
		if length < 2 || pos+prefix+length > end {
			return f
		}
		typ := dovi.NALType(hdr[lengthSize:])
		wanted := (typ == dovi.NALUnspec62 && f.rpu == nil) ||
			(typ == dovi.NALPrefixSEI && !f.hdr10Plus)
		if wanted && length <= maxNALBody {
			if err := c.seek(pos + prefix); err != nil {
				return f
			}
			nal, err := c.read(length)
			if err != nil {
				return f
			}
			if typ == dovi.NALUnspec62 {
				if r, err := dovi.ParseRPU(nal); err == nil {
					f.rpu = &r
				}
			} else if dovi.IsHDR10Plus(nal) {
				f.hdr10Plus = true
			}
		}
		pos += prefix + length
	}
	return f
}

// applyFrameFacts folds what the first frame said into the video record.
func applyFrameFacts(v *VideoInfo, cfg *dovi.Config, f frameFacts) {
	v.ApplyDolbyVision(cfg, f.rpu, f.hdr10Plus)
}

// ApplyDolbyVision records the Dolby Vision configuration record, the first
// RPU and whether HDR10+ metadata was seen, from whichever prober found them.
// It is the one merge step both the native walkers and the ffprobe mapping
// use, so the two cannot describe the same file differently (ADR 0031 §2).
func (v *VideoInfo) ApplyDolbyVision(cfg *dovi.Config, rpu *dovi.RPU, hdr10Plus bool) {
	if v == nil {
		return
	}
	if d := newDoVi(cfg, rpu); d != nil {
		v.DoVi = d
		v.HDR = append(v.HDR, DV)
	}
	if hdr10Plus {
		// ST 2094-40 is defined over a PQ signal: HDR10+ is HDR10 with
		// dynamic metadata, so the SEI proves the HDR10 base even when the
		// container wrote no colour box.
		v.HDR = append(v.HDR, HDR10Plus, HDR10)
	}
	v.HDR = normalizeHDR(v.HDR)
}
