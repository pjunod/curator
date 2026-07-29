package mediainfo_test

// Odd-but-legal container shapes, and the plausibility API the app layer
// calls. The corpus in corpus_test.go covers what an encoder normally emits;
// this covers what the spec permits and a real muxer occasionally does — the
// 64-bit box form, the version-1 headers, the extends-to-EOF box — plus the
// malformed variants of each.
//
// The standing rule from ADR 0013 §2 is that none of this may panic: this
// package reads a stranger's bytes on a background job in the process that
// serves the UI. So every case here asserts an outcome, and the table at the
// bottom asserts only "did not panic and did not lie", because a corrupt file
// is allowed to fail.

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// box64 writes the 64-bit box form: size field 1, type, then the real size as
// a u64. Muxers use it for the mdat of a long film, and a walker that cannot
// step over one never reaches the moov behind it.
func box64(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := make([]byte, 16, 16+len(body))
	binary.BigEndian.PutUint32(out, 1)
	copy(out[4:], typ)
	binary.BigEndian.PutUint64(out[8:], uint64(16+len(body)))
	return append(out, body...)
}

// ftypBox is the brand box Probe sniffs for; a file that does not open with
// one is not recognised as MP4 at all, so every hand-built case needs it.
func ftypBox() []byte {
	return box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2avc1mp41"))
}

// mvhd64 is the version-1 movie header: the time fields widened to 64 bits.
func mvhd64(timescale uint32, duration uint64) []byte {
	body := make([]byte, 112)
	body[0] = 1 // version
	// version+flags(4) creation(8) modification(8) timescale(4) duration(8)
	binary.BigEndian.PutUint32(body[20:], timescale)
	binary.BigEndian.PutUint64(body[24:], duration)
	return box("mvhd", body)
}

// A film long enough to need version 1, behind a 64-bit mdat. Both features
// have to work at once or the duration — which the bitrate and every
// plausibility rule is derived from — comes out zero.
func TestMI2MP4WideBoxesAndV1Header(t *testing.T) {
	// ftyp, then a 64-bit mdat the walker has to step over, then moov.
	file := ftypBox()
	file = append(file, box64("mdat", make([]byte, 32))...)
	file = append(file, box("moov",
		mvhd64(1000, 10_800_000), // three hours at millisecond timescale
		trak("vide", box("stsd", stsd(visualEntry("avc1", 1920, 1080)))),
	)...)

	info, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.DurationMS != 10_800_000 {
		t.Errorf("duration = %d ms, want 10800000 — the v1 mvhd was misread", info.DurationMS)
	}
	if info.Video == nil || info.Video.Width != 1920 {
		t.Fatalf("video = %+v", info.Video)
	}
}

// A 64-bit box whose declared length is nonsense must end the walk, not send
// the cursor somewhere arbitrary or allocate on the claim.
func TestMI2MP4RejectsAbsurdWideBox(t *testing.T) {
	for _, tc := range []struct {
		name  string
		large uint64
	}{
		{"smaller than its own header", 8},
		{"larger than any disk", math.MaxUint64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := make([]byte, 16)
			binary.BigEndian.PutUint32(hdr, 1)
			copy(hdr[4:], "mdat")
			binary.BigEndian.PutUint64(hdr[8:], tc.large)
			file := append(ftypBox(), hdr...)
			file = append(file, make([]byte, 32)...)

			if _, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file))); err == nil {
				t.Error("Probe accepted a box with an impossible 64-bit size")
			}
		})
	}
}

// A box with size 0 runs to end of file. It is legal as the last box, and the
// walker must stop there rather than treating 0 as "step nowhere" and looping.
func TestMI2MP4ZeroSizedTrailingBox(t *testing.T) {
	trailing := make([]byte, 8)
	copy(trailing[4:], "free") // size stays 0
	file := append(buildMP4(
		mvhd(1000, 60_000),
		trak("vide", box("stsd", stsd(visualEntry("hvc1", 3840, 2160)))),
	), append(trailing, make([]byte, 16)...)...)

	info, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.ResolutionTier() != 2160 {
		t.Errorf("tier = %d, want 2160", info.ResolutionTier())
	}

	// The same box in front of moov: the walk stops at it, so moov is never
	// found and the probe fails rather than reporting a hollow result.
	lead := append(ftypBox(), trailing...)
	lead = append(lead, box("moov", mvhd(1000, 60_000))...)
	if _, err := mediainfo.Probe(bytes.NewReader(lead), int64(len(lead))); err == nil {
		t.Error("a size-0 box before moov should end the walk with a failure")
	}
}

// mdhd carries the audio track's language, in both a v0 and a v1 layout. A
// misread offset yields three garbage letters, which is why the packed decode
// rejects anything outside a-z rather than showing it.
func TestMI2MP4TrackLanguage(t *testing.T) {
	// "eng" packed as three 5-bit values, each minus 0x60.
	packed := uint16('e'-0x60)<<10 | uint16('n'-0x60)<<5 | uint16('g'-0x60)

	mdhd := func(version byte, lang uint16) []byte {
		size := 24
		if version == 1 {
			size = 36
		}
		body := make([]byte, size)
		body[0] = version
		binary.BigEndian.PutUint16(body[size-4:], lang)
		return box("mdhd", body)
	}
	audioTrak := func(mdhdBox []byte) []byte {
		hdlrBody := make([]byte, 24)
		copy(hdlrBody[8:], "soun")
		return box("trak", box("mdia",
			box("hdlr", hdlrBody),
			mdhdBox,
			box("minf", box("stbl", box("stsd", stsd(audioEntry("mp4a", 2))))),
		))
	}

	for _, tc := range []struct {
		name string
		mdhd []byte
		want string
	}{
		{"v0", mdhd(0, packed), "eng"},
		{"v1 widens the time fields", mdhd(1, packed), "eng"},
		// 0x7FFF decodes to three characters outside a-z; better to report no
		// language than a word that is not one.
		{"unpacked garbage", mdhd(0, 0x7FFF), ""},
		{"truncated mdhd", box("mdhd", make([]byte, 6)), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := buildMP4(mvhd(1000, 60_000), audioTrak(tc.mdhd))
			info, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file)))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if len(info.Audio) != 1 {
				t.Fatalf("audio tracks = %d, want 1", len(info.Audio))
			}
			if info.Audio[0].Language != tc.want {
				t.Errorf("language = %q, want %q", info.Audio[0].Language, tc.want)
			}
		})
	}
}

// colr is MP4's transfer function. The 'nclc' spelling predates 'nclx' and
// carries the same fields; an ICC payload ('prof') carries none of them and
// must not be read as if it did.
func TestMI2MP4ColourBox(t *testing.T) {
	colr := func(kind string, transfer uint16) []byte {
		body := make([]byte, 11)
		copy(body, kind)
		binary.BigEndian.PutUint16(body[6:], transfer)
		return box("colr", body)
	}

	for _, tc := range []struct {
		name string
		box  []byte
		want []string
	}{
		{"nclx PQ is HDR10", colr("nclx", 16), []string{mediainfo.HDR10}},
		{"nclc PQ is HDR10 too", colr("nclc", 16), []string{mediainfo.HDR10}},
		{"HLG", colr("nclx", 18), []string{mediainfo.HLG}},
		{"SDR transfer says nothing", colr("nclx", 1), nil},
		{"an ICC profile is not a transfer function", colr("prof", 16), nil},
		{"truncated colr", box("colr", []byte("nclx")), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := buildMP4(
				mvhd(1000, 60_000),
				trak("vide", box("stsd", stsd(visualEntry("hvc1", 3840, 2160, tc.box)))),
			)
			info, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file)))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if info.Video == nil {
				t.Fatal("no video track")
			}
			if !equalStrings(info.Video.HDR, tc.want) {
				t.Errorf("HDR = %v, want %v", info.Video.HDR, tc.want)
			}
		})
	}
}

// Matroska stores Duration as a float, and both IEEE widths are legal. A
// muxer that writes the 4-byte form must not lose the duration — bitrate and
// every plausibility rule hang off it.
func TestMI2MatroskaFloatDurations(t *testing.T) {
	f32 := func(v float32) []byte {
		out := make([]byte, 4)
		binary.BigEndian.PutUint32(out, math.Float32bits(v))
		return out
	}
	track := ebml(0xAE,
		ebml(0x83, be(1)),
		ebml(0x86, []byte("V_MPEG4/ISO/AVC")),
		ebml(0xE0, ebml(0xB0, be(1920)), ebml(0xBA, be(1080))),
	)

	for _, tc := range []struct {
		name     string
		duration []byte
		want     int64
	}{
		{"8-byte float", f64(90_000), 90_000},
		{"4-byte float", f32(90_000), 90_000},
		// A zero-length float is legal EBML for 0.0, and a zero duration
		// means "unknown", not "instantaneous".
		{"empty float", nil, 0},
		// Neither width: unreadable, so no duration rather than a garbage one.
		{"5-byte float", []byte{1, 2, 3, 4, 5}, 0},
		{"NaN", f64(math.NaN()), 0},
		{"+Inf", f64(math.Inf(1)), 0},
		{"negative", f64(-5000), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := buildMatroska(
				ebml(0x1549A966,
					ebml(0x2AD7B1, be(1_000_000)), // TimecodeScale: 1 ms
					ebml(0x4489, tc.duration),
				),
				ebml(0x1654AE6B, track),
			)
			info, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file)))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if info.DurationMS != tc.want {
				t.Errorf("duration = %d ms, want %d", info.DurationMS, tc.want)
			}
		})
	}
}

// Truncated says "the file is cut short" using two known lengths, and
// MissingBytes is what the reason string quotes. The slack exists so a muxer
// finishing a hair under its own reservation is not accused of anything.
func TestMI2TruncationArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		declared  int64
		size      int64
		truncated bool
		missing   int64
	}{
		{"whole", 1_000_000, 1_000_000, false, 0},
		{"a hair short is within slack", 1_000_000, 999_000, false, 0},
		{"half the file never arrived", 1_000_000, 500_000, true, 500_000},
		{"no declared length to compare", 0, 500_000, false, 0},
		{"no size to compare", 1_000_000, 0, false, 0},
		{"longer than declared is not truncation", 500_000, 1_000_000, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := mediainfo.Info{DeclaredBytes: tc.declared, SizeBytes: tc.size}
			if got := i.Truncated(); got != tc.truncated {
				t.Errorf("Truncated = %v, want %v", got, tc.truncated)
			}
			if got := i.MissingBytes(); got != tc.missing {
				t.Errorf("MissingBytes = %d, want %d", got, tc.missing)
			}
		})
	}
}

// Check is the single entry point the API layer calls: the measurement-only
// rules first, then the one that needs the item's runtime. The order matters —
// a truncated file must be reported as truncated, not as merely short, because
// the two lead to different actions (blocklist versus go look at the download
// client).
func TestMI2CheckCombinesEveryRule(t *testing.T) {
	sound := mediainfo.Info{
		Container:  "mkv",
		Video:      &mediainfo.VideoInfo{Codec: "hevc", Width: 1920, Height: 1080},
		Audio:      []mediainfo.AudioInfo{{Codec: "eac3", Channels: 6}},
		DurationMS: 90 * 60_000, BitrateKbps: 8_000,
		SizeBytes: 5 << 30,
	}

	if why, bad := mediainfo.Check(sound, 90); bad {
		t.Errorf("a plausible file was rejected: %+v", why)
	}
	// Runtime unknown skips the duration rule rather than failing it.
	if why, bad := mediainfo.Check(sound, 0); bad {
		t.Errorf("runtime 0 should skip the duration rule, got %+v", why)
	}

	short := sound
	short.DurationMS = 5 * 60_000
	why, bad := mediainfo.Check(short, 90)
	if !bad || why.Code != mediainfo.CodeDurationShort {
		t.Errorf("a five-minute sample of a 90-minute feature = %+v/%v", why, bad)
	}
	if !strings.Contains(why.Reason, "90 minutes") {
		t.Errorf("reason should name the expected runtime: %q", why.Reason)
	}

	// A file that is both cut short and short on runtime reports the
	// truncation, because that is the one that says what to do about it.
	cut := sound
	cut.DeclaredBytes = 20 << 30
	cut.DurationMS = 5 * 60_000
	why, bad = mediainfo.Check(cut, 90)
	if !bad || why.Code != mediainfo.CodeTruncated {
		t.Errorf("truncation must outrank a short duration, got %+v/%v", why, bad)
	}

	// An unmeasured file is not implausible, it is unmeasured.
	if why, bad := mediainfo.Check(mediainfo.Info{}, 90); bad {
		t.Errorf("an unmeasured file was called implausible: %+v", why)
	}
}

// SizeImplausible is the pre-download twin: arithmetic on a tracker's number,
// before a byte is fetched. It has to fire on the obvious fakes and stay
// silent on everything else — a false positive here rejects a release nobody
// else was going to offer.
func TestMI2SizeImplausible(t *testing.T) {
	const gb = int64(1) << 30

	for _, tc := range []struct {
		name    string
		claimed quality.Quality
		size    int64
		runtime int
		want    bool
	}{
		{
			// The headline case: a 2160p remux is a lossless disc copy, and
			// 2 GB over two hours cannot be one.
			name:    "a 2 GB 2160p remux is not a remux",
			claimed: quality.Quality{Source: quality.SourceRemux, Resolution: 2160},
			size:    2 * gb, runtime: 120, want: true,
		},
		{
			name:    "a 60 GB 2160p remux is exactly what one looks like",
			claimed: quality.Quality{Source: quality.SourceRemux, Resolution: 2160},
			size:    60 * gb, runtime: 120, want: false,
		},
		{
			// Below the impossibility floor, whatever the claimed source.
			name:    "300 MB claiming 2160p",
			claimed: quality.Quality{Source: quality.SourceWEBDL, Resolution: 2160},
			size:    300 << 20, runtime: 120, want: true,
		},
		{
			name:    "a lean but real 1080p web encode",
			claimed: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
			size:    2 * gb, runtime: 120, want: false,
		},
		{
			// A season pack's runtime is per-episode while the size covers
			// many, so the computed bitrate comes out far too high and the
			// rule must simply not fire. This can miss; it must not misfire.
			name:    "a season pack is never judged",
			claimed: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
			size:    40 * gb, runtime: 45, want: false,
		},
		{
			name:    "no size to judge",
			claimed: quality.Quality{Source: quality.SourceRemux, Resolution: 2160},
			size:    0, runtime: 120, want: false,
		},
		{
			name:    "no runtime to judge",
			claimed: quality.Quality{Source: quality.SourceRemux, Resolution: 2160},
			size:    2 * gb, runtime: 0, want: false,
		},
		{
			// An unrecognised resolution has no floor to compare against, so
			// the claim gets the benefit of the doubt.
			name:    "no resolution claimed",
			claimed: quality.Quality{Source: quality.SourceWEBDL},
			size:    100 << 20, runtime: 120, want: false,
		},
		{
			// Rounds to 0 kbps: nothing to judge rather than a divide that
			// makes everything implausible.
			name:    "a size so small the bitrate rounds away",
			claimed: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
			size:    1000, runtime: 120, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why, bad := mediainfo.SizeImplausible(tc.claimed, tc.size, tc.runtime)
			if bad != tc.want {
				t.Fatalf("SizeImplausible = %v (%+v), want %v", bad, why, tc.want)
			}
			if !bad {
				return
			}
			if why.Code != mediainfo.CodeSizeTooSmall {
				t.Errorf("code = %q, want %q", why.Code, mediainfo.CodeSizeTooSmall)
			}
			// The reason is shown to a user deciding whether to override, so
			// it has to name the numbers it judged on.
			if !strings.Contains(why.Reason, "minutes") || why.Reason == "" {
				t.Errorf("reason does not explain itself: %q", why.Reason)
			}
		})
	}
}

// Provenance is persisted, so the vocabulary is a storage contract: Valid is
// what stops a typo being written, and Measured is what "the pixels were
// read" means everywhere else.
func TestMI2ProvenanceVocabulary(t *testing.T) {
	for _, p := range []mediainfo.Provenance{
		mediainfo.ProvenanceUnknown, mediainfo.ProvenanceProbe,
		mediainfo.ProvenanceFilename, mediainfo.ProvenanceRelease,
		mediainfo.ProvenanceManual, mediainfo.ProvenanceFailed,
		mediainfo.ProvenanceImplausible,
	} {
		if !p.Valid() {
			t.Errorf("Provenance(%q).Valid() = false", p)
		}
		if p.Label() == "" {
			t.Errorf("Provenance(%q) has no badge text", p)
		}
	}
	if mediainfo.Provenance("probed").Valid() {
		t.Error("a typo passed Valid() and would be persisted")
	}

	// Only a probe means measured. Manual is authoritative but nobody read
	// the pixels, and that distinction is what the don't-churn rule turns on.
	if !mediainfo.ProvenanceProbe.Measured() {
		t.Error("ProvenanceProbe.Measured() = false")
	}
	for _, p := range []mediainfo.Provenance{
		mediainfo.ProvenanceManual, mediainfo.ProvenanceFilename,
		mediainfo.ProvenanceRelease, mediainfo.ProvenanceFailed,
		mediainfo.ProvenanceImplausible, mediainfo.ProvenanceUnknown,
	} {
		if p.Measured() {
			t.Errorf("Provenance(%q).Measured() = true", p)
		}
	}

	for _, c := range []mediainfo.Confidence{
		mediainfo.ConfidenceNone, mediainfo.ConfidenceHigh,
		mediainfo.ConfidenceMedium, mediainfo.ConfidenceLow,
	} {
		if !c.Valid() {
			t.Errorf("Confidence(%q).Valid() = false", c)
		}
	}
	if mediainfo.Confidence("very").Valid() {
		t.Error("a typo passed Confidence.Valid()")
	}
}

// Resolve with nothing measured: the row must say the claim came from the
// name, not dress a guess up as a measurement.
func TestMI2ResolveWithoutMeasurement(t *testing.T) {
	q, prov, conf := mediainfo.Resolve(mediainfo.Info{},
		quality.Quality{Source: quality.SourceBluray, Resolution: 1080},
		mediainfo.ProvenanceFilename)
	if q.Source != quality.SourceBluray || q.Resolution != 1080 {
		t.Errorf("quality = %+v, want the claim kept intact", q)
	}
	if prov != mediainfo.ProvenanceFilename || conf != mediainfo.ConfidenceNone {
		t.Errorf("provenance/confidence = %q/%q, want filename/none", prov, conf)
	}

	// A resolution-only claim still counts as a claim, and an empty source
	// normalises to the explicit unknown rather than "".
	q, prov, _ = mediainfo.Resolve(mediainfo.Info{}, quality.Quality{Resolution: 720},
		mediainfo.ProvenanceRelease)
	if q.Source != quality.SourceUnknown || q.Resolution != 720 {
		t.Errorf("quality = %+v, want unknown/720", q)
	}
	if prov != mediainfo.ProvenanceRelease {
		t.Errorf("provenance = %q, want release", prov)
	}

	// Nothing measured and nothing claimed: the file exists and could not be
	// read, which is its own state (ADR 0013 §5).
	q, prov, _ = mediainfo.Resolve(mediainfo.Info{}, quality.Quality{}, mediainfo.ProvenanceUnknown)
	if q.Source != quality.SourceUnknown || prov != mediainfo.ProvenanceFailed {
		t.Errorf("quality/provenance = %+v/%q, want unknown/failed", q, prov)
	}
}

// The malformed-container table. These are the shapes that reach Probe from a
// partial download, a truncated copy or a file that is not what its extension
// says. None may panic; each must either fail or return something honest.
//
// Kept as a table so a new crasher found by FuzzProbe can be pinned here as a
// named case rather than only as a corpus file.
func TestMI2MalformedContainersNeverPanic(t *testing.T) {
	track := ebml(0xAE,
		ebml(0x83, be(1)),
		ebml(0x86, []byte("V_MPEG4/ISO/AVC")),
		ebml(0xE0, ebml(0xB0, be(1920)), ebml(0xBA, be(1080))),
	)
	good := buildMatroska(
		ebml(0x1549A966, ebml(0x2AD7B1, be(1_000_000)), ebml(0x4489, f64(60_000))),
		ebml(0x1654AE6B, track),
	)
	goodMP4 := buildMP4(
		mvhd(1000, 60_000),
		trak("vide", box("stsd", stsd(visualEntry("avc1", 1920, 1080)))),
	)

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"an EBML magic and nothing else", []byte{0x1A, 0x45, 0xDF, 0xA3}},
		{"unterminated vint size", []byte{0x1A, 0x45, 0xDF, 0xA3, 0x00, 0x00, 0x00}},
		{"mkv cut mid-header", good[:len(good)/3]},
		{"mkv cut just before the tracks", good[:len(good)-4]},
		{"mkv with a zeroed body", append(append([]byte{}, good[:16]...), make([]byte, 64)...)},
		{"mp4 cut mid-moov", goodMP4[:len(goodMP4)-8]},
		{"mp4 header only", goodMP4[:16]},
		{"ftyp with no moov behind it", []byte("\x00\x00\x00\x10ftypisom\x00\x00\x02\x00")},
		{"a box claiming more than the file holds",
			append(ftypBox(), []byte("\x7f\xff\xff\xffmoov\x00\x00\x00\x00")...)},
		{"a box smaller than its own header",
			append(ftypBox(), []byte("\x00\x00\x00\x03moov\x00\x00\x00\x00")...)},
		{"an stsd claiming a million entries", buildMP4(
			mvhd(1000, 60_000),
			trak("vide", box("stsd", append(
				[]byte{0, 0, 0, 0, 0x00, 0x0F, 0x42, 0x40},
				visualEntry("avc1", 1920, 1080)...))),
		)},
		{"a visual entry with no body", buildMP4(
			mvhd(1000, 60_000),
			trak("vide", box("stsd", stsd(box("avc1")))),
		)},
		{"an mkv track with no codec id", buildMatroska(
			ebml(0x1654AE6B, ebml(0xAE, ebml(0x83, be(1)),
				ebml(0xE0, ebml(0xB0, be(1920)), ebml(0xBA, be(1080))))),
		)},
		{"an mkv dimension beyond any display", buildMatroska(
			ebml(0x1654AE6B, ebml(0xAE, ebml(0x83, be(1)),
				ebml(0x86, []byte("V_MPEG4/ISO/AVC")),
				ebml(0xE0, ebml(0xB0, be(1<<40)), ebml(0xBA, be(1<<40))))),
		)},
		{"text that is not a container at all", []byte(strings.Repeat("not a video ", 64))},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := mediainfo.Probe(bytes.NewReader(tc.data), int64(len(tc.data)))
			// Failure is fine. Lying is not: "measured" has to mean a video
			// track with real dimensions was read.
			if info.Measured() && (info.Video == nil || info.Video.Width <= 0 || info.Video.Height <= 0) {
				t.Fatalf("Measured() with video %+v (err %v)", info.Video, err)
			}
			if info.Video != nil && (info.Video.Width < 0 || info.Video.Height < 0) {
				t.Fatalf("negative dimensions: %+v", info.Video)
			}
			if info.DurationMS < 0 || info.BitrateKbps < 0 {
				t.Fatalf("negative duration/bitrate: %d/%d", info.DurationMS, info.BitrateKbps)
			}
			// Summary is rendered into the UI; it must be empty unless
			// something was actually measured.
			if s := info.Summary(); s != "" && !info.Measured() {
				t.Fatalf("Summary %q on an unmeasured file", s)
			}
			// And an unmeasured file is never implausible — it is unmeasured,
			// which is handled elsewhere.
			if !info.Measured() {
				if why, bad := mediainfo.Check(info, 90); bad {
					t.Fatalf("unmeasured file judged implausible: %+v", why)
				}
			}
		})
	}
}

// audioEntry builds an AudioSampleEntry. The 28-byte preamble layout is
// documented in mp4.go; channelcount sits at offset 16.
func audioEntry(format string, channels int, subBoxes ...[]byte) []byte {
	body := make([]byte, 28)
	binary.BigEndian.PutUint16(body[16:], uint16(channels))
	return box(format, append(body, flatten(subBoxes)...))
}
