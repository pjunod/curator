// Package dovi decodes the parts of a Dolby Vision stream that say what kind
// of Dolby Vision it is: the decoder configuration record a container stores
// once, and the RPU (reference processing unit) that rides with every frame.
//
// It is pure and bounded like the rest of mediainfo: it is handed bytes, never
// a file, and it must never panic on arbitrary input. Both probe paths — the
// native container walkers and the ffprobe mapping — reduce to the facts here,
// so a file cannot get two different answers depending on which one read it
// (ADR 0031 §2).
//
// The RPU syntax follows the published libdovi parser (MIT, quietvoid), which
// is the de facto reference implementation; the fixtures in testdata were
// produced by it and the tests compare against its parsed output.
package dovi

// Config is the Dolby Vision decoder configuration record: the 24-byte
// payload of an MP4 dvcC/dvvC/dvwC box, and of a Matroska
// BlockAdditionMapping whose BlockAddIDType names one of those.
type Config struct {
	VersionMajor int
	VersionMinor int
	Profile      int
	Level        int
	RPU          bool // rpu_present_flag
	EL           bool // el_present_flag: an enhancement layer exists
	BL           bool // bl_present_flag
	// BLCompatID is dv_bl_signal_compatibility_id — what a player that does
	// not decode Dolby Vision gets from the base layer: 0 none (profile 5),
	// 1 HDR10, 2 SDR, 4 HLG, 6 HDR10 as authored for UHD Blu-ray.
	BLCompatID int
}

// ParseConfig decodes a configuration record. It needs only the first five
// bytes; the rest of the 24 are reserved.
//
//	byte 0  dv_version_major
//	byte 1  dv_version_minor
//	byte 2  dv_profile(7) dv_level(high 1)
//	byte 3  dv_level(low 5) rpu(1) el(1) bl(1)
//	byte 4  dv_bl_signal_compatibility_id(4) reserved(4)
func ParseConfig(b []byte) (Config, bool) {
	if len(b) < 5 {
		return Config{}, false
	}
	c := Config{
		VersionMajor: int(b[0]),
		VersionMinor: int(b[1]),
		Profile:      int(b[2] >> 1),
		Level:        int(b[2]&1)<<5 | int(b[3]>>3),
		RPU:          b[3]&0x04 != 0,
		EL:           b[3]&0x02 != 0,
		BL:           b[3]&0x01 != 0,
		BLCompatID:   int(b[4] >> 4),
	}
	// A record with no layers at all, or a profile number from outside the
	// defined range, is not a configuration record — it is a box that
	// happened to be named one.
	if c.Profile > 20 || (!c.RPU && !c.EL && !c.BL) {
		return Config{}, false
	}
	return c, true
}
