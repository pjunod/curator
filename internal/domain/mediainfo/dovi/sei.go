package dovi

// HEVC NAL unit types this package recognises.
const (
	NALPrefixSEI = 39
	NALUnspec62  = 62 // Dolby Vision RPU
	NALUnspec63  = 63 // Dolby Vision enhancement layer, in single-track profile 7
)

// NALType returns the HEVC nal_unit_type of a NAL unit (header included).
func NALType(nal []byte) int {
	if len(nal) < 2 {
		return -1
	}
	return int(nal[0]>>1) & 0x3F
}

// IsHDR10Plus reports whether an HEVC prefix SEI NAL unit carries SMPTE ST
// 2094-40 dynamic metadata — HDR10+. The marker is a registered ITU-T T.35
// payload: country 0xB5 (United States), provider 0x003C (Samsung), provider
// oriented code 0x0001, application identifier 4.
func IsHDR10Plus(nal []byte) bool {
	if NALType(nal) != NALPrefixSEI {
		return false
	}
	b := unescape(nal[2:])
	for i := 0; i < len(b); {
		if b[i] == 0x80 && i == len(b)-1 { // rbsp trailing bits
			return false
		}
		ptype, n := seiValue(b[i:])
		if n == 0 {
			return false
		}
		i += n
		psize, n := seiValue(b[i:])
		if n == 0 {
			return false
		}
		i += n
		if psize > len(b)-i {
			return false
		}
		p := b[i : i+psize]
		if ptype == 4 && len(p) >= 6 &&
			p[0] == 0xB5 && p[1] == 0x00 && p[2] == 0x3C &&
			p[3] == 0x00 && p[4] == 0x01 && p[5] == 0x04 {
			return true
		}
		i += psize
	}
	return false
}

// seiValue reads an SEI payload type or size: a run of 0xFF bytes each adding
// 255, then a final byte. It returns the value and bytes consumed (0 on
// truncation).
func seiValue(b []byte) (int, int) {
	v := 0
	for i, x := range b {
		v += int(x)
		if x != 0xFF {
			return v, i + 1
		}
		if v > 1<<20 {
			return 0, 0
		}
	}
	return 0, 0
}
