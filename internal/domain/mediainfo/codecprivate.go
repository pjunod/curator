package mediainfo

// Codec configuration records — hvcC, avcC, av1C — are the same bytes in MKV
// (CodecPrivate) and MP4 (a box inside the sample entry), so both walkers share
// this file. All we want from them is bit depth: 10-bit is a fact about the
// file that no filename reliably carries, and it is the difference between a
// modern HDR encode and an SDR one at the same resolution.
//
// Every reader here is bounds-checked and returns 0 rather than guessing. A
// truncated or nonsense codec record is a normal thing to meet on a user's
// disk, not an error worth failing a probe over.

// bitDepthFromCodecPrivate dispatches on the already-resolved codec name.
func bitDepthFromCodecPrivate(codec string, private []byte) int {
	switch codec {
	case "hevc":
		return bitDepthHVCC(private)
	case "h264":
		return bitDepthAVCC(private)
	case "av1":
		return bitDepthAV1C(private)
	}
	return 0
}

// bitDepthHVCC reads HEVCDecoderConfigurationRecord (ISO/IEC 14496-15 §8.3.3.1).
// Layout up to the field we want, one byte per line unless noted:
//
//	0      configurationVersion
//	1      profile_space(2) tier_flag(1) profile_idc(5)
//	2-5    general_profile_compatibility_flags(32)
//	6-11   general_constraint_indicator_flags(48)
//	12     general_level_idc
//	13-14  reserved(4) min_spatial_segmentation_idc(12)
//	15     reserved(6) parallelismType(2)
//	16     reserved(6) chromaFormat(2)
//	17     reserved(5) bitDepthLumaMinus8(3)   <- here
func bitDepthHVCC(b []byte) int {
	if len(b) < 18 || b[0] != 1 {
		return 0
	}
	return int(b[17]&0x07) + 8
}

// bitDepthAVCC reads AVCDecoderConfigurationRecord. Bit depth only exists in
// the optional trailing extension, and only for the high profiles that can
// carry more than 8 bits — which means walking past the parameter sets:
//
//	0 configurationVersion · 1 AVCProfileIndication · 2 profile_compatibility
//	3 AVCLevelIndication   · 4 reserved(6) lengthSizeMinusOne(2)
//	5 reserved(3) numOfSequenceParameterSets(5), then each: length(2) + data
//	  numOfPictureParameterSets(1), then each: length(2) + data
//	  [if profile in {100,110,122,144}] chroma(1) bitDepthLumaMinus8(1) …
func bitDepthAVCC(b []byte) int {
	if len(b) < 7 || b[0] != 1 {
		return 0
	}
	profile := b[1]
	switch profile {
	case 100, 110, 122, 144, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
	default:
		return 8 // profiles that cannot exceed 8-bit
	}
	r := &byteReader{b: b, pos: 5}
	numSPS, ok := r.u8()
	if !ok {
		return 0
	}
	for i := 0; i < int(numSPS&0x1F); i++ {
		n, ok := r.u16()
		if !ok || !r.skip(int(n)) {
			return 0
		}
	}
	numPPS, ok := r.u8()
	if !ok {
		return 0
	}
	for i := 0; i < int(numPPS); i++ {
		n, ok := r.u16()
		if !ok || !r.skip(int(n)) {
			return 0
		}
	}
	if r.left() < 4 {
		return 8 // extension absent: the record predates it, so 8-bit
	}
	r.skip(1) // reserved(6) chroma_format_idc(2)
	depth, ok := r.u8()
	if !ok {
		return 0
	}
	return int(depth&0x07) + 8
}

// bitDepthAV1C reads AV1CodecConfigurationRecord:
//
//	0 marker(1) version(7)
//	1 seq_profile(3) seq_level_idx_0(5)
//	2 seq_tier_0(1) high_bitdepth(1) twelve_bit(1) monochrome(1) …
func bitDepthAV1C(b []byte) int {
	if len(b) < 3 || b[0]&0x80 == 0 {
		return 0
	}
	profile := (b[1] >> 5) & 0x07
	high := b[2]&0x40 != 0
	twelve := b[2]&0x20 != 0
	switch {
	case profile == 2 && high && twelve:
		return 12
	case high:
		return 10
	}
	return 8
}
