package mediainfo

// Internal tests for the pure decode helpers: codec vocabularies, codec-record
// bit depth, and the bounds-checked byte reader everything else is built on.
//
// These are worth testing directly rather than only through Probe because a
// wrong answer here is silent. A CodecID that maps to the wrong name does not
// fail a probe — it changes which audio floor plausibility uses and which band
// inference lands in, and the file quietly gets the wrong verdict. And a
// byteReader that runs off the end of a buffer is a panic on a background scan,
// which is the one thing ADR 0013 §2 says must never happen.

import "testing"

// The Matroska CodecID vocabulary. The ordering inside the switch is
// load-bearing in two places — A_AC3/BSID10 must be read as E-AC-3 before the
// A_AC3 prefix catches it, and A_MLP must reach truehd — so the table names
// both explicitly.
func TestMI2MatroskaCodecIDs(t *testing.T) {
	video := map[string]string{
		"V_MPEG4/ISO/AVC":  "h264",
		"V_MPEG4/ISO/avc3": "h264",
		"V_MPEGH/ISO/HEVC": "hevc",
		"V_AV1":            "av1",
		"V_VP9":            "vp9",
		"V_VP8":            "vp8",
		"V_MPEG2":          "mpeg2",
		"V_MPEG1":          "mpeg1",
		"V_MPEG4/ISO/ASP":  "mpeg4",
		"V_MS/VFW/FOURCC":  "vc1",
		"V_THEORA":         "theora",
		"":                 "",
		// Anything unrecognised keeps its own name, lowercased and
		// unprefixed, rather than becoming "" — an unknown codec is still a
		// fact worth showing.
		"V_SOMETHING_NEW": "something_new",
	}
	for id, want := range video {
		if got := matroskaVideoCodec(id); got != want {
			t.Errorf("matroskaVideoCodec(%q) = %q, want %q", id, got, want)
		}
	}

	audio := map[string]string{
		"A_TRUEHD":      "truehd",
		"A_MLP":         "truehd",
		"A_DTS":         "dts",
		"A_DTS/EXPRESS": "dts",
		// BSID 10/11 is E-AC-3 wearing an AC-3 CodecID. Reading it as plain
		// AC-3 mislabels a modern web release as a broadcast-era one, which
		// is exactly the distinction source inference leans on.
		"A_AC3/BSID10":   "eac3",
		"A_AC3/BSID11":   "eac3",
		"A_EAC3":         "eac3",
		"A_AC3":          "ac3",
		"A_AAC":          "aac",
		"A_AAC/MPEG4/LC": "aac",
		"A_FLAC":         "flac",
		"A_PCM/INT/LIT":  "pcm",
		"A_ALAC":         "alac",
		"A_OPUS":         "opus",
		"A_VORBIS":       "vorbis",
		"A_MPEG/L3":      "mp3",
		"A_MPEG/L2":      "mp2",
		"":               "",
		"A_WEIRD":        "weird",
	}
	for id, want := range audio {
		if got := matroskaAudioCodec(id); got != want {
			t.Errorf("matroskaAudioCodec(%q) = %q, want %q", id, got, want)
		}
	}
}

// Dolby Vision is sometimes only in the CodecID, with no BlockAdditionMapping
// to find. Missing it means an HDR file shows as SDR.
func TestMI2DVFromCodecID(t *testing.T) {
	for _, id := range []string{"V_MPEGH/ISO/HEVC/DVHE", "v_dolbyvision", "DVH1"} {
		if !dvFromCodecID(id) {
			t.Errorf("dvFromCodecID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"V_MPEGH/ISO/HEVC", "A_TRUEHD", ""} {
		if dvFromCodecID(id) {
			t.Errorf("dvFromCodecID(%q) = true, want false", id)
		}
	}
}

// The MP4 sample-entry vocabulary, including the DV-flavoured formats that
// carry both a codec and an HDR fact.
func TestMI2MP4SampleFormats(t *testing.T) {
	for _, tc := range []struct {
		format string
		codec  string
		dv     bool
	}{
		{"avc1", "h264", false},
		{"avc3", "h264", false},
		{"dva1", "h264", true},
		{"dvav", "h264", true},
		{"hvc1", "hevc", false},
		{"hev1", "hevc", false},
		{"dvh1", "hevc", true},
		{"dvhe", "hevc", true},
		{"av01", "av1", false},
		{"dav1", "av1", true},
		{"vp09", "vp9", false},
		{"vp08", "vp8", false},
		{"mp4v", "mpeg4", false},
		{"mp2v", "mpeg2", false},
		{"vc-1", "vc1", false},
		// Case is not guaranteed on the wire.
		{"HVC1", "hevc", false},
		// Unknown formats yield nothing rather than a guess: the caller skips
		// the entry and looks at the next one.
		{"mp4s", "", false},
		{"", "", false},
	} {
		codec, dv := mp4VideoCodec(tc.format)
		if codec != tc.codec || dv != tc.dv {
			t.Errorf("mp4VideoCodec(%q) = %q/%v, want %q/%v", tc.format, codec, dv, tc.codec, tc.dv)
		}
	}

	audio := map[string]string{
		"mp4a": "aac", "ec-3": "eac3", "ac-3": "ac3", "ac-4": "ac4",
		"dtsc": "dts", "dtsx": "dts", "mlpa": "truehd", "flac": "flac",
		"opus": "opus", "alac": "alac", "sowt": "pcm", "lpcm": "pcm",
		"raw ": "pcm", ".mp3": "mp3", "mp3 ": "mp3",
		"EC-3": "eac3", "tx3g": "", "": "",
	}
	for format, want := range audio {
		if got := mp4AudioCodec(format); got != want {
			t.Errorf("mp4AudioCodec(%q) = %q, want %q", format, got, want)
		}
	}
}

// acmod is how (E-)AC-3 states its channel layout, and it is the only honest
// source for it — the AudioSampleEntry channel count says 2 for a 5.1 track.
// The 5.1 case (acmod 7 + LFE) is the one that decides "surround" in the UI.
func TestMI2ACMODChannels(t *testing.T) {
	for acmod, want := range map[int]int{
		0: 2, // dual mono
		1: 1,
		2: 2,
		3: 3, 4: 3,
		5: 4, 6: 4,
		7: 5,
	} {
		if got := acmodChannels(acmod); got != want {
			t.Errorf("acmodChannels(%d) = %d, want %d", acmod, got, want)
		}
	}
	// Out of range: report nothing rather than a made-up count.
	for _, acmod := range []int{-1, 8, 99} {
		if got := acmodChannels(acmod); got != 0 {
			t.Errorf("acmodChannels(%d) = %d, want 0", acmod, got)
		}
	}

	// setChannels only overwrites when the layout actually decodes, so a
	// nonsense acmod leaves whatever the sample entry said.
	a := &AudioInfo{Codec: "eac3", Channels: 2}
	setChannels(a, 99, 0)
	if a.Channels != 2 {
		t.Errorf("an undecodable acmod overwrote channels: %d", a.Channels)
	}
	setChannels(a, 7, 1)
	if a.Channels != 6 {
		t.Errorf("acmod 7 + LFE = %d channels, want 6 (5.1)", a.Channels)
	}
}

// Bit depth is the fact no filename carries reliably, and 10-bit is what
// separates a modern HDR encode from an SDR one at the same resolution. Every
// reader must return 0 on a record it cannot trust rather than a plausible
// wrong number.
func TestMI2BitDepthFromCodecRecords(t *testing.T) {
	t.Run("hvcC", func(t *testing.T) {
		rec := make([]byte, 18)
		rec[0] = 1
		rec[17] = 0xF8 | 0x02 // reserved bits set, bitDepthLumaMinus8 = 2
		if got := bitDepthHVCC(rec); got != 10 {
			t.Errorf("bitDepthHVCC = %d, want 10", got)
		}
		rec[0] = 2 // unknown configurationVersion
		if got := bitDepthHVCC(rec); got != 0 {
			t.Errorf("a foreign hvcC version returned %d, want 0", got)
		}
		if got := bitDepthHVCC(rec[:17]); got != 0 {
			t.Errorf("a truncated hvcC returned %d, want 0", got)
		}
	})

	t.Run("av1C", func(t *testing.T) {
		// marker bit set, version 1; profile in bits 7-5 of byte 1.
		mk := func(profile byte, high, twelve bool) []byte {
			b := []byte{0x81, profile << 5, 0x00}
			if high {
				b[2] |= 0x40
			}
			if twelve {
				b[2] |= 0x20
			}
			return b
		}
		for _, tc := range []struct {
			name string
			rec  []byte
			want int
		}{
			{"8-bit", mk(0, false, false), 8},
			{"10-bit", mk(0, true, false), 10},
			// twelve_bit only means 12 in professional profile 2; elsewhere
			// it is a reserved bit and high_bitdepth still means 10.
			{"12-bit needs profile 2", mk(2, true, true), 12},
			{"twelve_bit outside profile 2 is still 10", mk(0, true, true), 10},
			{"no marker bit", []byte{0x01, 0x00, 0x00}, 0},
			{"truncated", []byte{0x81, 0x00}, 0},
		} {
			if got := bitDepthAV1C(tc.rec); got != tc.want {
				t.Errorf("%s: bitDepthAV1C = %d, want %d", tc.name, got, tc.want)
			}
		}
	})

	t.Run("avcC", func(t *testing.T) {
		// A baseline/main profile cannot exceed 8-bit, so the walk is skipped
		// entirely — no parameter sets to step over.
		if got := bitDepthAVCC([]byte{1, 66, 0, 30, 0xFF, 0xE0, 0}); got != 8 {
			t.Errorf("baseline profile = %d, want 8", got)
		}
		if got := bitDepthAVCC([]byte{2, 100, 0, 30, 0xFF, 0xE0, 0}); got != 0 {
			t.Errorf("a foreign avcC version = %d, want 0", got)
		}
		if got := bitDepthAVCC([]byte{1, 100}); got != 0 {
			t.Errorf("a truncated avcC = %d, want 0", got)
		}

		// High profile with the trailing extension: the reader has to walk
		// past one SPS and one PPS to reach the bit-depth byte.
		high := func(ext []byte) []byte {
			rec := []byte{
				1, 100, 0, 40, 0xFF,
				0xE1,       // numOfSequenceParameterSets = 1
				0x00, 0x03, // SPS length
				0xAA, 0xBB, 0xCC,
				0x01,       // numOfPictureParameterSets = 1
				0x00, 0x02, // PPS length
				0xDD, 0xEE,
			}
			return append(rec, ext...)
		}
		// chroma(1) bitDepthLuma(1) bitDepthChroma(1) numSPSExt(1)
		if got := bitDepthAVCC(high([]byte{0xFD, 0xF8 | 0x02, 0xF8, 0x00})); got != 10 {
			t.Errorf("high profile 10-bit = %d, want 10", got)
		}
		if got := bitDepthAVCC(high([]byte{0xFD, 0xF8, 0xF8, 0x00})); got != 8 {
			t.Errorf("high profile 8-bit = %d, want 8", got)
		}
		// Extension absent: the record predates it, which means 8-bit, not
		// "unknown". Claiming unknown would drop the depth from the summary
		// of every older high-profile encode.
		if got := bitDepthAVCC(high(nil)); got != 8 {
			t.Errorf("high profile without the extension = %d, want 8", got)
		}
		// A parameter-set length that runs past the buffer must abort, not
		// read whatever follows.
		if got := bitDepthAVCC([]byte{1, 100, 0, 40, 0xFF, 0xE1, 0x7F, 0xFF, 0xAA}); got != 0 {
			t.Errorf("an overlong SPS length = %d, want 0", got)
		}
	})

	// The dispatcher only knows three codecs; anything else has no record to
	// read and must say so.
	if got := bitDepthFromCodecPrivate("vp9", []byte{1, 2, 3}); got != 0 {
		t.Errorf("bitDepthFromCodecPrivate(vp9) = %d, want 0", got)
	}
	if got := bitDepthFromCodecPrivate("av1", []byte{0x81, 0x00, 0x40}); got != 10 {
		t.Errorf("bitDepthFromCodecPrivate(av1) = %d, want 10", got)
	}
}

// byteReader is the bounds discipline every in-memory walker inherits. A read
// that runs past the end must report failure, never panic and never return
// bytes from beyond the slice.
func TestMI2ByteReaderBounds(t *testing.T) {
	r := &byteReader{b: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}}
	if v, ok := r.u8(); !ok || v != 0x01 {
		t.Fatalf("u8 = %v/%v", v, ok)
	}
	if v, ok := r.u16(); !ok || v != 0x0203 {
		t.Fatalf("u16 = %v/%v", v, ok)
	}
	if v, ok := r.u32(); !ok || v != 0x04050607 {
		t.Fatalf("u32 = %v/%v", v, ok)
	}
	if r.left() != 1 {
		t.Fatalf("left = %d, want 1", r.left())
	}
	if _, ok := r.u16(); ok {
		t.Error("u16 succeeded with one byte left")
	}
	if _, ok := r.u32(); ok {
		t.Error("u32 succeeded with one byte left")
	}

	// u64 is how a 64-bit box size is read; it must fail cleanly on a short
	// buffer in both halves rather than composing garbage.
	wide := &byteReader{b: []byte{0, 0, 0, 0, 0, 0, 0x01, 0x00}}
	if v, ok := wide.u64(); !ok || v != 256 {
		t.Errorf("u64 = %v/%v, want 256/true", v, ok)
	}
	if _, ok := (&byteReader{b: []byte{1, 2, 3}}).u64(); ok {
		t.Error("u64 succeeded on 3 bytes")
	}
	if _, ok := (&byteReader{b: []byte{1, 2, 3, 4, 5}}).u64(); ok {
		t.Error("u64 succeeded on 5 bytes (low half short)")
	}

	// A negative or overlong length is a corrupt size field, not a request.
	br := &byteReader{b: []byte{1, 2, 3, 4}}
	if _, ok := br.bytes(-1); ok {
		t.Error("bytes(-1) succeeded")
	}
	if _, ok := br.bytes(99); ok {
		t.Error("bytes(99) succeeded on a 4-byte buffer")
	}
	if got, ok := br.bytes(4); !ok || len(got) != 4 {
		t.Errorf("bytes(4) = %v/%v", got, ok)
	}
	// A failed skip parks at the end so the caller's loop terminates instead
	// of spinning on the same position.
	sk := &byteReader{b: []byte{1, 2, 3, 4}}
	if sk.skip(99) {
		t.Error("skip(99) succeeded")
	}
	if sk.left() != 0 {
		t.Errorf("a failed skip left %d bytes; the walker would loop", sk.left())
	}
	if (&byteReader{b: []byte{1}}).skip(-1) {
		t.Error("skip(-1) succeeded")
	}
}

// The cursor is the budget enforcement for on-disk reads: a corrupt length
// field claiming 2^60 bytes must be refused before a single allocation.
func TestMI2CursorRefusesAbsurdReads(t *testing.T) {
	data := make([]byte, 64)
	c := newCursor(bytesReaderAt(data), int64(len(data)), 32)

	if _, err := c.read(-1); err != ErrMalformed {
		t.Errorf("read(-1) err = %v, want ErrMalformed", err)
	}
	if b, err := c.read(0); err != nil || b != nil {
		t.Errorf("read(0) = %v/%v, want nil/nil", b, err)
	}
	if _, err := c.read(1 << 60); err == nil {
		t.Error("read(2^60) succeeded")
	}
	if _, err := c.read(16); err != nil {
		t.Fatalf("read(16): %v", err)
	}
	// 16 spent of a 32-byte budget; asking for 32 more must hit the budget,
	// not the file size.
	if _, err := c.read(32); err != ErrBudget {
		t.Errorf("over-budget read err = %v, want ErrBudget", err)
	}

	// skip moves without spending budget, and refuses to leave the file.
	c2 := newCursor(bytesReaderAt(data), int64(len(data)), 8)
	if err := c2.skip(60); err != nil {
		t.Fatalf("skip(60): %v", err)
	}
	if c2.remaining() != 4 {
		t.Errorf("remaining = %d, want 4", c2.remaining())
	}
	if err := c2.skip(-1); err == nil {
		t.Error("skip(-1) succeeded")
	}
	if err := c2.skip(100); err == nil {
		t.Error("skip past EOF succeeded")
	}
	if err := c2.seek(-1); err == nil {
		t.Error("seek(-1) succeeded")
	}
	if err := c2.seek(int64(len(data)) + 1); err == nil {
		t.Error("seek past EOF succeeded")
	}
	// Seeking backwards is legal and free — MP4's moov can sit behind us.
	if err := c2.seek(0); err != nil {
		t.Errorf("seek(0): %v", err)
	}
}

// The small formatting helpers that end up in an operator-facing reason
// string. resolutionName has to survive a tier of 0, which is what an
// unrecognised claim produces.
func TestMI2ReasonFormatting(t *testing.T) {
	if got := resolutionName(2160); got != "2160p" {
		t.Errorf("resolutionName(2160) = %q", got)
	}
	if got := resolutionName(0); got != "that resolution" {
		t.Errorf("resolutionName(0) = %q, want a phrase that reads in a sentence", got)
	}
	if got := resolutionName(-5); got != "that resolution" {
		t.Errorf("resolutionName(-5) = %q", got)
	}

	for _, tc := range []struct {
		bytes int64
		want  string
	}{
		{5 << 30, "5.0 GB"},
		{700 << 20, "700 MB"},
		{512 * 1024, "512 KB"},
		{0, "0 KB"},
	} {
		if got := humanSize(tc.bytes); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}

	if got := formatMinutes(90 * 60_000); got != "90 minutes" {
		t.Errorf("formatMinutes = %q", got)
	}
	// Under a minute reads in seconds; "0 minutes" would be useless in the
	// reason a user sees.
	if got := formatMinutes(45_000); got != "45 seconds" {
		t.Errorf("formatMinutes(45s) = %q", got)
	}
}

// clampDimension and normalizeLanguage guard the two fields a corrupt header
// most easily poisons.
func TestMI2FieldClamping(t *testing.T) {
	if got := clampDimension(1920); got != 1920 {
		t.Errorf("clampDimension(1920) = %d", got)
	}
	// Beyond any real display: report nothing rather than a dimension that
	// would blow past every resolution tier.
	if got := clampDimension(1 << 40); got != 0 {
		t.Errorf("clampDimension(2^40) = %d, want 0", got)
	}
	if got := clampDimension(65535); got != 65535 {
		t.Errorf("clampDimension(65535) = %d", got)
	}

	for in, want := range map[string]string{
		"eng":   "eng",
		"ENG":   "eng",
		" en ":  "en",
		"en-US": "en",
		"pt_BR": "pt",
		"und":   "",
		"":      "",
		"-x":    "-x", // no prefix to cut: index 0 is not a separator
	} {
		if got := normalizeLanguage(in); got != want {
			t.Errorf("normalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

// bytesReaderAt is a minimal io.ReaderAt over a slice; the cursor only needs
// ReadAt, and using bytes.Reader here would drag the dependency into an
// internal test for no benefit.
type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, errShortRead
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, errShortRead
	}
	return n, nil
}

var errShortRead = errShort{}

type errShort struct{}

func (errShort) Error() string { return "short read" }
