package dovi

import (
	"errors"
	"fmt"
)

// Enhancement-layer types. Empty means the probe could not tell, which is
// not the same as "none" (ADR 0026 §1).
const (
	ELNone = "none" // single layer (profile 5, 8, …)
	ELMEL  = "mel"  // minimal enhancement layer: carries no residual picture
	ELFEL  = "fel"  // full enhancement layer: real residual picture data
)

// Content-mapping versions, named the way the industry writes them.
const (
	CM29 = "2.9"
	CM40 = "4.0"
)

// ErrNotRPU means the bytes are not a Dolby Vision RPU at all.
var ErrNotRPU = errors.New("dovi: not an RPU")

// RPU is what one reference processing unit says about the stream.
type RPU struct {
	Profile   int
	ELType    string // ELNone | ELMEL | ELFEL | "" (unknown: the RPU reused a previous mapping)
	CMVersion string // CM29 | CM40 | "" (no display-management payload)

	// Level 6 metadata: the HDR10-style static values, in nits. HasL6 says
	// whether the block was present at all, since zero is a legal MaxCLL.
	HasL6               bool
	MaxDisplayMastering int
	MinDisplayMastering int // in 0.0001 nits, as the block stores it
	MaxCLL              int
	MaxFALL             int
}

// Header holds the RPU header fields profile classification reads. It is
// exported so the ffprobe mapping, which receives these fields already
// decoded, applies exactly the same rule as the native parser.
type Header struct {
	VDRRPUProfile       int
	BLFullRange         bool
	ELSpatialResampling bool
	DisableResidual     bool
	VDRBitDepth         int // vdr_bit_depth_minus8 + 8
}

// Profile infers the Dolby Vision profile from the RPU header, the same way
// libdovi's get_dovi_profile does. 0 means the header fits no profile.
func (h Header) Profile() int {
	switch h.VDRRPUProfile {
	case 0:
		if h.BLFullRange {
			return 5
		}
		return 0
	case 1:
		if h.ELSpatialResampling && !h.DisableResidual {
			if h.VDRBitDepth == 12 {
				return 7
			}
			return 4
		}
		return 8
	}
	return 0
}

// NLQ is the non-linear quantizer each colour component carries when the
// enhancement layer has a residual, in the split integer/fraction form the
// bitstream codes it.
type NLQ struct {
	Offset       [3]uint64
	VDRInMaxInt  [3]uint64
	VDRInMax     [3]uint64
	SlopeInt     [3]uint64
	Slope        [3]uint64
	ThresholdInt [3]uint64
	Threshold    [3]uint64
}

// IsMEL reports whether the quantizer is the identity a minimal enhancement
// layer writes: zero offset, unit input range, zero dead-zone. Any other value
// means the EL contributes picture data, which is what FEL is.
func (n NLQ) IsMEL() bool {
	for c := 0; c < 3; c++ {
		if n.Offset[c] != 0 || n.VDRInMaxInt[c] != 1 || n.VDRInMax[c] != 0 ||
			n.SlopeInt[c] != 0 || n.Slope[c] != 0 ||
			n.ThresholdInt[c] != 0 || n.Threshold[c] != 0 {
			return false
		}
	}
	return true
}

// ELType classifies the quantizer.
func (n NLQ) ELType() string {
	if n.IsMEL() {
		return ELMEL
	}
	return ELFEL
}

// Bounds the parser enforces before looping on a length the stream supplied.
// They are far above anything a conformant stream writes and far below
// anything that could cost real time.
const (
	maxPivots    = 9
	maxExtBlocks = 64
	maxBitDepth  = 16
	mmrMaxCoeffs = 7
)

// ParseRPU decodes an RPU. It accepts the forms RPUs are found in: an HEVC
// UNSPEC62 NAL unit (header 7C 01), and the dovi_tool RPU.bin form that drops
// the NAL header and starts at the 0x19 prefix; either may carry an Annex B
// start code. Both are emulation-prevention escaped, as everything that came
// out of an HEVC stream is.
func ParseRPU(b []byte) (RPU, error) {
	b = trimStartCode(b)
	switch {
	case len(b) >= 2 && b[0] == 0x7C && b[1] == 0x01:
		b = unescape(b[2:])
	case len(b) >= 1 && b[0] == 0x19:
		b = unescape(b)
	default:
		return RPU{}, ErrNotRPU
	}
	end := len(b)
	for end > 0 && b[end-1] == 0 {
		end--
	}
	// prefix(1) + something + CRC32(4) + 0x80 terminator(1)
	if end < 8 || b[end-1] != 0x80 {
		return RPU{}, fmt.Errorf("%w: bad terminator", ErrNotRPU)
	}
	b = b[:end]
	want := uint32(b[end-5])<<24 | uint32(b[end-4])<<16 | uint32(b[end-3])<<8 | uint32(b[end-2])
	if got := crc32MPEG2(b[1 : end-5]); got != want {
		return RPU{}, fmt.Errorf("%w: CRC mismatch", ErrNotRPU)
	}
	return parseRPUData(b)
}

func trimStartCode(b []byte) []byte {
	switch {
	case len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1:
		return b[4:]
	case len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1:
		return b[3:]
	}
	return b
}

// rpuHeader is the full header as the bitstream codes it.
type rpuHeader struct {
	Header
	rpuFormat     uint64
	seqInfo       bool
	coefType      uint64
	denomLen      int
	blBitDepth    int
	elBitDepth    int
	reservedZero3 uint64
	dmPresent     bool
	usePrev       bool
}

func parseRPUData(b []byte) (RPU, error) {
	r := newBitReader(b)
	if r.bits(8) != 0x19 {
		return RPU{}, ErrNotRPU
	}
	h, err := parseHeader(r)
	if err != nil {
		return RPU{}, err
	}
	out := RPU{Profile: h.Profile()}

	if !h.usePrev {
		el, err := parseMapping(r, &h)
		if err != nil {
			return RPU{}, err
		}
		out.ELType = el
	}
	if h.dmPresent {
		if err := parseDM(r, &h, &out); err != nil {
			return RPU{}, err
		}
	}
	if !r.ok() {
		return RPU{}, fmt.Errorf("%w: truncated", ErrNotRPU)
	}
	return out, nil
}

func parseHeader(r *bitReader) (rpuHeader, error) {
	var h rpuHeader
	if r.bits(6) != 2 { // rpu_type
		return h, fmt.Errorf("%w: rpu_type", ErrNotRPU)
	}
	h.rpuFormat = r.bits(11)
	h.VDRRPUProfile = int(r.bits(4))
	r.bits(4) // vdr_rpu_level
	h.seqInfo = r.flag()
	h.blBitDepth, h.elBitDepth, h.VDRBitDepth = 8, 8, 8
	if h.seqInfo {
		r.flag() // chroma_resampling_explicit_filter_flag
		h.coefType = r.bits(2)
		var denom uint64
		if h.coefType == 0 {
			denom = r.ue()
		}
		r.bits(2) // vdr_rpu_normalized_idc
		h.BLFullRange = r.flag()
		if h.rpuFormat&0x700 == 0 {
			bl := r.ue()
			el := r.ue() & 0xFF // high bits carry ext_mapping_idc
			vdr := r.ue()
			if bl > maxBitDepth-8 || el > maxBitDepth-8 || vdr > maxBitDepth-8 {
				return h, fmt.Errorf("%w: bit depth", ErrNotRPU)
			}
			h.blBitDepth, h.elBitDepth, h.VDRBitDepth = int(bl)+8, int(el)+8, int(vdr)+8
			r.flag() // spatial_resampling_filter_flag
			h.reservedZero3 = r.bits(3)
			h.ELSpatialResampling = r.flag()
			h.DisableResidual = r.flag()
		}
		switch h.coefType {
		case 0:
			if denom > 32 {
				return h, fmt.Errorf("%w: coefficient_log2_denom", ErrNotRPU)
			}
			h.denomLen = int(denom)
		case 1:
			h.denomLen = 32
		default:
			return h, fmt.Errorf("%w: coefficient_data_type", ErrNotRPU)
		}
	}
	h.dmPresent = r.flag()
	h.usePrev = r.flag()
	if h.usePrev {
		r.ue() // prev_vdr_rpu_id
	}
	if !r.ok() {
		return h, fmt.Errorf("%w: truncated header", ErrNotRPU)
	}
	return h, nil
}

// parseMapping walks rpu_data_mapping and returns the enhancement-layer type.
// Every coefficient must be read to reach the quantizer at the end, even
// though only the quantizer is kept.
func parseMapping(r *bitReader, h *rpuHeader) (string, error) {
	r.ue() // vdr_rpu_id
	r.ue() // mapping_color_space
	r.ue() // mapping_chroma_format_idc

	var pieces [3]int
	for c := 0; c < 3; c++ {
		n := r.ue() + 2
		if n > maxPivots || !r.ok() {
			return "", fmt.Errorf("%w: pivots", ErrNotRPU)
		}
		pieces[c] = int(n) - 1
		r.skip(int(n) * h.blBitDepth)
	}
	hasNLQ := h.rpuFormat&0x700 == 0 && !h.DisableResidual
	if hasNLQ {
		if r.bits(3) != 0 { // nlq_method_idc: only linear dead-zone exists
			return "", fmt.Errorf("%w: nlq_method_idc", ErrNotRPU)
		}
		r.skip(2 * h.blBitDepth) // nlq_pred_pivot_value
	}
	r.ue() // num_x_partitions_minus1
	r.ue() // num_y_partitions_minus1

	coef := func(signed bool) {
		if h.coefType == 0 {
			if signed {
				r.se()
			} else {
				r.ue()
			}
		}
		r.skip(h.denomLen)
	}
	for c := 0; c < 3; c++ {
		for p := 0; p < pieces[c]; p++ {
			switch r.ue() { // mapping_idc
			case 0: // polynomial
				order := r.ue()
				if order > 1 {
					return "", fmt.Errorf("%w: poly_order", ErrNotRPU)
				}
				if order == 0 && r.flag() {
					// linear_interp_flag: no known encoder writes it and
					// libdovi does not implement it either.
					return "", fmt.Errorf("%w: linear interpolation unsupported", ErrNotRPU)
				}
				for j := 0; j < int(order)+2; j++ {
					coef(true)
				}
			case 1: // MMR
				order := r.bits(2)
				if order > 2 {
					return "", fmt.Errorf("%w: mmr_order", ErrNotRPU)
				}
				coef(true) // mmr_constant
				for j := 0; j < int(order)+1; j++ {
					for k := 0; k < mmrMaxCoeffs; k++ {
						coef(true)
					}
				}
			default:
				return "", fmt.Errorf("%w: mapping_idc", ErrNotRPU)
			}
			if !r.ok() {
				return "", fmt.Errorf("%w: truncated mapping", ErrNotRPU)
			}
		}
	}
	if !hasNLQ {
		return ELNone, nil
	}
	var n NLQ
	for c := 0; c < 3; c++ {
		n.Offset[c] = r.bits(h.elBitDepth)
		if h.coefType == 0 {
			n.VDRInMaxInt[c] = r.ue()
		}
		n.VDRInMax[c] = r.bits(h.denomLen)
		if h.coefType == 0 {
			n.SlopeInt[c] = r.ue()
		}
		n.Slope[c] = r.bits(h.denomLen)
		if h.coefType == 0 {
			n.ThresholdInt[c] = r.ue()
		}
		n.Threshold[c] = r.bits(h.denomLen)
	}
	if h.coefType != 0 {
		// Floating-point coefficients have no integer part on the wire; the
		// MEL identity is then 1.0f for vdr_in_max, which is not a split
		// form. No shipping encoder writes this, so do not guess.
		return "", nil
	}
	if !r.ok() {
		return "", fmt.Errorf("%w: truncated nlq", ErrNotRPU)
	}
	return n.ELType(), nil
}

// parseDM reads vdr_dm_data far enough to know the content-mapping version and
// the level 6 static metadata.
func parseDM(r *bitReader, h *rpuHeader, out *RPU) error {
	if h.reservedZero3 == 1 { // compressed DM: ids only
		r.ue()
		r.ue()
		r.ue()
	} else {
		r.ue()                               // affected_dm_metadata_id
		r.ue()                               // current_dm_metadata_id
		r.ue()                               // scene_refresh_flag
		r.skip(9*16 + 3*32 + 9*16)           // ycc_to_rgb, offsets, rgb_to_lms
		r.skip(16 + 16 + 16 + 32)            // signal_eotf and params
		r.skip(5 + 2 + 2 + 2 + 12 + 12 + 10) // signal_* and source_*
	}
	if !r.ok() {
		return fmt.Errorf("%w: truncated dm", ErrNotRPU)
	}
	if _, err := extBlocks(r, out); err != nil {
		return err
	}
	out.CMVersion = CM29
	// A second block list follows only in CM v4.0 streams. libdovi's
	// threshold: room for a level 254 block before the CRC and terminator.
	if r.left() >= 16+40 {
		n, err := extBlocks(r, out)
		if err != nil {
			return err
		}
		if n > 0 {
			out.CMVersion = CM40
		}
	}
	return nil
}

// extBlocks reads one ext_metadata block list, keeping level 6.
func extBlocks(r *bitReader, out *RPU) (int, error) {
	n := r.ue()
	if n > maxExtBlocks {
		return 0, fmt.Errorf("%w: ext blocks", ErrNotRPU)
	}
	for !r.aligned() {
		if r.flag() {
			return 0, fmt.Errorf("%w: dm alignment", ErrNotRPU)
		}
	}
	for i := 0; i < int(n); i++ {
		length := r.ue()
		level := r.bits(8)
		if !r.ok() || length > uint64(r.left()/8) {
			return 0, fmt.Errorf("%w: ext block length", ErrNotRPU)
		}
		bitsLeft := int(length) * 8
		if level == 6 && length >= 8 {
			out.HasL6 = true
			out.MaxDisplayMastering = int(r.bits(16))
			out.MinDisplayMastering = int(r.bits(16))
			out.MaxCLL = int(r.bits(16))
			out.MaxFALL = int(r.bits(16))
			bitsLeft -= 64
		}
		r.skip(bitsLeft)
	}
	if !r.ok() {
		return 0, fmt.Errorf("%w: truncated ext blocks", ErrNotRPU)
	}
	return int(n), nil
}
