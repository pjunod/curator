package dovi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The RPU fixtures were written by libdovi (dovi_tool's dolby_vision crate)
// for plurx's renderer experiments, and the expectations below are libdovi's
// own parse of the same bytes: dovi_profile, el_type, whether a CM v4.0 block
// list is present, and the level 6 block.
//
//	.nal  HEVC UNSPEC62 NAL unit (7C 01 header, emulation prevention)
//	.bin  bare RPU payload with an Annex B start code
func TestParseRPUAgainstLibdovi(t *testing.T) {
	cases := []struct {
		file    string
		profile int
		el      string
		cm      string
	}{
		{"p7_fel_linear.nal", 7, ELFEL, CM40},
		{"p7_fel_linear.bin", 7, ELFEL, CM40},
		{"p7_fel_bounded_residual.nal", 7, ELFEL, CM40},
		{"p7_fel_nonstandard_linear_matrix.nal", 7, ELFEL, CM40},
		{"p81_identity.nal", 8, ELNone, CM40},
		{"p81_identity.bin", 8, ELNone, CM40},
		{"p81_adapted_linear.nal", 8, ELNone, CM40},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParseRPU(b)
			if err != nil {
				t.Fatalf("ParseRPU: %v", err)
			}
			if got.Profile != tc.profile || got.ELType != tc.el || got.CMVersion != tc.cm {
				t.Errorf("got profile %d el %q cm %q, want %d %q %q",
					got.Profile, got.ELType, got.CMVersion, tc.profile, tc.el, tc.cm)
			}
			if !got.HasL6 || got.MaxDisplayMastering != 1000 || got.MinDisplayMastering != 1 ||
				got.MaxCLL != 0 || got.MaxFALL != 0 {
				t.Errorf("L6 = %+v, want mastering 1000/1, MaxCLL 0, MaxFALL 0", got)
			}
		})
	}
}

func TestParseRPURejectsTruncated(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "truncated.nal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRPU(b); !errors.Is(err, ErrNotRPU) {
		t.Fatalf("ParseRPU(truncated) error = %v, want ErrNotRPU", err)
	}
	// Every prefix of a valid RPU must fail cleanly rather than parse as
	// something else or panic.
	full, err := os.ReadFile(filepath.Join("testdata", "p7_fel_linear.nal"))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(full)-1; n++ {
		if _, err := ParseRPU(full[:n]); err == nil {
			t.Fatalf("prefix of %d bytes parsed", n)
		}
	}
}

func TestParseRPURejectsCorruptCRC(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "p81_identity.bin"))
	if err != nil {
		t.Fatal(err)
	}
	b = append([]byte(nil), b...)
	b[20] ^= 0x10
	if _, err := ParseRPU(b); !errors.Is(err, ErrNotRPU) {
		t.Fatalf("corrupted RPU error = %v, want ErrNotRPU", err)
	}
}

// TestParseRPUMinimalEnhancementLayer builds a profile 7 RPU whose quantizer is
// the MEL identity, with a CM v2.9 display-management payload only. libdovi
// fixtures cover FEL and single-layer streams; this covers the other EL type
// and the older CM version, written from the syntax table.
func TestParseRPUMinimalEnhancementLayer(t *testing.T) {
	for _, mel := range []bool{true, false} {
		got, err := ParseRPU(buildRPU(t, mel))
		if err != nil {
			t.Fatalf("mel=%v: %v", mel, err)
		}
		want := ELFEL
		if mel {
			want = ELMEL
		}
		if got.Profile != 7 || got.ELType != want || got.CMVersion != CM29 {
			t.Errorf("mel=%v: got %+v, want profile 7 %s CM 2.9", mel, got, want)
		}
		if !got.HasL6 || got.MaxCLL != 1200 || got.MaxFALL != 400 || got.MaxDisplayMastering != 4000 {
			t.Errorf("mel=%v: L6 = %+v", mel, got)
		}
	}
}

func TestHeaderProfile(t *testing.T) {
	cases := []struct {
		h    Header
		want int
	}{
		{Header{VDRRPUProfile: 0, BLFullRange: true}, 5},
		{Header{VDRRPUProfile: 0}, 0},
		{Header{VDRRPUProfile: 1, ELSpatialResampling: true, VDRBitDepth: 12}, 7},
		{Header{VDRRPUProfile: 1, ELSpatialResampling: true, VDRBitDepth: 10}, 4},
		{Header{VDRRPUProfile: 1, DisableResidual: true, VDRBitDepth: 12}, 8},
		{Header{VDRRPUProfile: 3}, 0},
	}
	for _, tc := range cases {
		if got := tc.h.Profile(); got != tc.want {
			t.Errorf("%+v.Profile() = %d, want %d", tc.h, got, tc.want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	// Profile 7, level 6, RPU+EL+BL, compatibility 6: the UHD Blu-ray record.
	p7 := []byte{1, 0, 7 << 1, 6<<3 | 0x07, 6 << 4} // level 6: top bit 0 in byte 2, low 5 bits in byte 3
	c, ok := ParseConfig(p7)
	if !ok || c.Profile != 7 || c.Level != 6 || !c.RPU || !c.EL || !c.BL || c.BLCompatID != 6 {
		t.Errorf("P7 record = %+v ok=%v", c, ok)
	}
	// Profile 8, level 33 (the high level bit is in byte 2), RPU+BL, compat 1.
	p8 := []byte{1, 0, 8<<1 | 1, 1<<3 | 0x05, 1 << 4}
	c, ok = ParseConfig(p8)
	if !ok || c.Profile != 8 || c.Level != 33 || !c.RPU || c.EL || !c.BL || c.BLCompatID != 1 {
		t.Errorf("P8 record = %+v ok=%v", c, ok)
	}
	if _, ok := ParseConfig(make([]byte, 24)); ok {
		t.Error("an all-zero record (no layers) parsed")
	}
	if _, ok := ParseConfig([]byte{1, 0, 2}); ok {
		t.Error("a 3-byte record parsed")
	}
}

func TestIsHDR10Plus(t *testing.T) {
	t35 := []byte{0xB5, 0x00, 0x3C, 0x00, 0x01, 0x04, 0x01, 0x40}
	sei := func(ptype byte, payload []byte) []byte {
		nal := []byte{NALPrefixSEI << 1, 0x01, ptype, byte(len(payload))}
		nal = append(nal, payload...)
		return append(nal, 0x80)
	}
	if !IsHDR10Plus(sei(4, t35)) {
		t.Error("ST 2094-40 T.35 payload not recognised")
	}
	// Two messages, HDR10+ second: mastering display (type 137) first.
	two := []byte{NALPrefixSEI << 1, 0x01, 137, 2, 0xAA, 0xBB, 4, byte(len(t35))}
	two = append(append(two, t35...), 0x80)
	if !IsHDR10Plus(two) {
		t.Error("HDR10+ as the second SEI message not recognised")
	}
	other := append([]byte(nil), t35...)
	other[2] = 0x3B // a different provider
	if IsHDR10Plus(sei(4, other)) {
		t.Error("another provider's T.35 payload read as HDR10+")
	}
	if IsHDR10Plus([]byte{NALUnspec62 << 1, 1, 4, 6}) {
		t.Error("a non-SEI NAL read as HDR10+")
	}
	if IsHDR10Plus(sei(4, t35)[:6]) {
		t.Error("a truncated SEI read as HDR10+")
	}
}

// bitWriter builds test bitstreams in the order the syntax tables read.
type bitWriter struct {
	b    []byte
	nbit int
}

func (w *bitWriter) put(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit%8 == 0 {
			w.b = append(w.b, 0)
		}
		if (v>>uint(i))&1 == 1 {
			w.b[len(w.b)-1] |= 1 << (7 - uint(w.nbit%8))
		}
		w.nbit++
	}
}

func (w *bitWriter) flag(f bool) {
	if f {
		w.put(1, 1)
	} else {
		w.put(0, 1)
	}
}

func (w *bitWriter) ue(v uint64) {
	v++
	n := 0
	for x := v; x > 1; x >>= 1 {
		n++
	}
	w.put(0, n)
	w.put(v, n+1)
}

func (w *bitWriter) se(v int64) {
	if v > 0 {
		w.ue(uint64(2*v - 1))
	} else {
		w.ue(uint64(-2 * v))
	}
}

func (w *bitWriter) align() {
	for w.nbit%8 != 0 {
		w.put(0, 1)
	}
}

// buildRPU writes a profile 7 RPU: identity polynomial mapping, a MEL or FEL
// quantizer, an uncompressed DM payload with CM v2.9 blocks L1 and L6.
func buildRPU(t *testing.T, mel bool) []byte {
	t.Helper()
	const denom = 23
	w := &bitWriter{}
	w.put(0x19, 8)
	// rpu_data_header
	w.put(2, 6)   // rpu_type
	w.put(18, 11) // rpu_format
	w.put(1, 4)   // vdr_rpu_profile
	w.put(0, 4)   // vdr_rpu_level
	w.flag(true)  // vdr_seq_info_present_flag
	w.flag(false) // chroma_resampling_explicit_filter_flag
	w.put(0, 2)   // coefficient_data_type
	w.ue(denom)   // coefficient_log2_denom
	w.put(1, 2)   // vdr_rpu_normalized_idc
	w.flag(false) // bl_video_full_range_flag
	w.ue(2)       // bl_bit_depth_minus8
	w.ue(2)       // el_bit_depth_minus8
	w.ue(4)       // vdr_bit_depth_minus8
	w.flag(false) // spatial_resampling_filter_flag
	w.put(0, 3)   // reserved_zero_3bits
	w.flag(true)  // el_spatial_resampling_filter_flag
	w.flag(false) // disable_residual_flag
	w.flag(true)  // vdr_dm_metadata_present_flag
	w.flag(false) // use_prev_vdr_rpu_flag
	// rpu_data_mapping
	w.ue(0) // vdr_rpu_id
	w.ue(0) // mapping_color_space
	w.ue(0) // mapping_chroma_format_idc
	for c := 0; c < 3; c++ {
		w.ue(0) // num_pivots_minus2
		w.put(0, 10)
		w.put(1023, 10)
	}
	w.put(0, 3) // nlq_method_idc
	w.put(0, 10)
	w.put(1023, 10)
	w.ue(0) // num_x_partitions_minus1
	w.ue(0) // num_y_partitions_minus1
	for c := 0; c < 3; c++ {
		w.ue(0)       // mapping_idc polynomial
		w.ue(0)       // poly_order_minus1
		w.flag(false) // linear_interp_flag
		w.se(0)
		w.put(0, denom)
		w.se(1)
		w.put(0, denom)
	}
	for c := 0; c < 3; c++ {
		if mel {
			w.put(0, 10) // nlq_offset
			w.ue(1)      // vdr_in_max_int
			w.put(0, denom)
			w.ue(0) // slope int
			w.put(0, denom)
			w.ue(0) // threshold int
			w.put(0, denom)
		} else {
			w.put(512, 10)
			w.ue(1)
			w.put(0, denom)
			w.ue(0)
			w.put(8192, denom)
			w.ue(0)
			w.put(4096, denom)
		}
	}
	// vdr_dm_data_payload, uncompressed
	w.ue(0)
	w.ue(0)
	w.ue(0)
	for i := 0; i < 9; i++ {
		w.put(0, 16)
	}
	for i := 0; i < 3; i++ {
		w.put(0, 32)
	}
	for i := 0; i < 9; i++ {
		w.put(0, 16)
	}
	w.put(65535, 16)
	w.put(0, 16)
	w.put(0, 16)
	w.put(0, 32)
	w.put(12, 5)
	w.put(0, 2)
	w.put(0, 2)
	w.put(1, 2)
	w.put(0, 12)
	w.put(4095, 12)
	w.put(42, 10)
	// CM v2.9 ext blocks: L1 (5 bytes: 3×12 bits + 4 pad) and L6 (8 bytes)
	w.ue(2)
	w.align()
	w.ue(5)
	w.put(1, 8)
	w.put(0, 12)
	w.put(2081, 12)
	w.put(1000, 12)
	w.put(0, 4)
	w.ue(8)
	w.put(6, 8)
	w.put(4000, 16)
	w.put(50, 16)
	w.put(1200, 16)
	w.put(400, 16)
	w.align()
	crc := crc32MPEG2(w.b[1:])
	w.put(uint64(crc), 32)
	w.put(0x80, 8)
	return append([]byte{0x7C, 0x01}, escape(w.b)...)
}

// escape inserts HEVC emulation-prevention bytes, as a muxer does.
func escape(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/2)
	zeros := 0
	for _, x := range b {
		if zeros >= 2 && x <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, x)
		if x == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
