package mediainfo_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// TestResolutionTier walks the tiering rule (plan §4.2) including the cases
// that motivated tiering on width OR height rather than height alone.
func TestResolutionTier(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		want int
	}{
		{"UHD", 3840, 2160, 2160},
		{"DCI 4K", 4096, 2160, 2160},
		{"UHD scope crop", 3840, 1600, 2160},
		{"pillarboxed 4K", 2880, 2160, 2160},
		{"1080p", 1920, 1080, 1080},
		{"1080p scope 2.39:1", 1920, 800, 1080},
		{"1080p extreme scope", 1920, 720, 1080},
		{"1440x1080 anamorphic", 1440, 1080, 1080},
		{"720p", 1280, 720, 720},
		{"720p scope", 1280, 534, 720},
		{"PAL DVD", 720, 576, 480},
		{"NTSC DVD", 720, 480, 480},
		{"widescreen SD", 854, 480, 480},
		{"nothing measured", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := mediainfo.Info{Video: &mediainfo.VideoInfo{Width: tc.w, Height: tc.h}}
			if got := info.ResolutionTier(); got != tc.want {
				t.Errorf("%dx%d tier = %d, want %d", tc.w, tc.h, got, tc.want)
			}
		})
	}
	if got := (mediainfo.Info{}).ResolutionTier(); got != 0 {
		t.Errorf("tier with no video track = %d, want 0", got)
	}
}

// TestDolbyVisionMatroska builds the smallest MKV that says "Dolby Vision" the
// Matroska way — a BlockAdditionMapping whose BlockAddIDType is 'dvcC' — and
// checks the walker finds it. No encoder we have access to produces DV, so the
// fixture is hand-built from the spec; that also makes the encoding legible.
func TestDolbyVisionMatroska(t *testing.T) {
	video := ebml(0xE0,
		ebml(0xB0, be(3840)), // PixelWidth
		ebml(0xBA, be(2160)), // PixelHeight
		ebml(0x55B0, // Colour
			ebml(0x55BA, be(16)), // TransferCharacteristics = PQ
		),
		ebml(0x41E4, // BlockAdditionMapping
			ebml(0x41E7, be(0x64766343)), // BlockAddIDType = 'dvcC'
		),
	)
	track := ebml(0xAE,
		ebml(0x83, be(1)), // TrackType = video
		ebml(0x86, []byte("V_MPEGH/ISO/HEVC")),
		video,
	)
	audio := ebml(0xAE,
		ebml(0x83, be(2)),
		ebml(0x86, []byte("A_TRUEHD")),
		ebml(0xE1, ebml(0x9F, be(8))),
	)
	file := buildMatroska(
		ebml(0x1549A966, // Info
			ebml(0x2AD7B1, be(1000000)),
			ebml(0x4489, f64(7200000)), // Duration in scale units = 7200 s
			ebml(0x5741, []byte("MakeMKV v1.17.5")),
		),
		ebml(0x1654AE6B, track, audio), // Tracks
	)

	info, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Video == nil {
		t.Fatal("no video track")
	}
	if got, want := info.Video.HDR, []string{mediainfo.DV, mediainfo.HDR10}; !equalStrings(got, want) {
		t.Errorf("HDR = %v, want %v", got, want)
	}
	if info.ResolutionTier() != 2160 {
		t.Errorf("tier = %d, want 2160", info.ResolutionTier())
	}
	if info.WritingApp != "MakeMKV v1.17.5" {
		t.Errorf("WritingApp = %q", info.WritingApp)
	}
	if info.DurationMS != 7200000 {
		t.Errorf("duration = %d ms, want 7200000", info.DurationMS)
	}
	if !info.HasLosslessAudio() {
		t.Error("TrueHD track not recognized as lossless")
	}
}

// TestDolbyVisionMP4 covers the other half of the DV story: MP4 says it with a
// 'dvh1' sample entry and a dvcC box.
func TestDolbyVisionMP4(t *testing.T) {
	dvcC := box("dvcC", make([]byte, 24))
	entry := visualEntry("dvh1", 3840, 2160, dvcC)
	file := buildMP4(
		mvhd(1000, 7_200_000),
		trak("vide", box("stsd", stsd(entry))),
	)

	info, err := mediainfo.Probe(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Video == nil {
		t.Fatal("no video track")
	}
	if info.Video.Codec != "hevc" {
		t.Errorf("codec = %q, want hevc (dvh1 is HEVC with a DV layer)", info.Video.Codec)
	}
	if got, want := info.Video.HDR, []string{mediainfo.DV}; !equalStrings(got, want) {
		t.Errorf("HDR = %v, want %v", got, want)
	}
	if info.DurationMS != 7_200_000 {
		t.Errorf("duration = %d ms, want 7200000", info.DurationMS)
	}
}

// TestAudioFamilies pins the classification source inference leans on. Getting
// these wrong is how a remux gets called a web release.
func TestAudioFamilies(t *testing.T) {
	cases := []struct {
		name                       string
		codecs                     []string
		lossless, disc, allWebOrNe bool
		best                       string
	}{
		{"TrueHD plus AC3", []string{"truehd", "ac3"}, true, false, false, "truehd"},
		{"FLAC only", []string{"flac"}, true, false, false, "flac"},
		{"DTS", []string{"dts"}, false, true, false, "dts"},
		{"EAC3 web", []string{"eac3"}, false, false, true, "eac3"},
		{"AAC only", []string{"aac"}, false, false, true, "aac"},
		{"AC3 neutral", []string{"ac3"}, false, false, true, "ac3"},
		{"no audio", nil, false, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var info mediainfo.Info
			for _, c := range tc.codecs {
				info.Audio = append(info.Audio, mediainfo.AudioInfo{Codec: c, Channels: 6})
			}
			if got := info.HasLosslessAudio(); got != tc.lossless {
				t.Errorf("HasLosslessAudio = %v, want %v", got, tc.lossless)
			}
			if got := info.HasDiscAudio(); got != tc.disc {
				t.Errorf("HasDiscAudio = %v, want %v", got, tc.disc)
			}
			if got := info.AudioAllWebOrNeutral(); got != tc.allWebOrNe {
				t.Errorf("AudioAllWebOrNeutral = %v, want %v", got, tc.allWebOrNe)
			}
			if got := info.BestAudio(); got != tc.best {
				t.Errorf("BestAudio = %q, want %q", got, tc.best)
			}
		})
	}
	web := mediainfo.Info{Audio: []mediainfo.AudioInfo{{Codec: "ac3"}}}
	if web.HasWebAudio() {
		t.Error("AC-3 alone counted as positively web-flavoured; it proves nothing")
	}
}

// TestProbeRejectsGarbage: arbitrary bytes must produce an error, never a
// measurement and never a panic.
func TestProbeRejectsGarbage(t *testing.T) {
	cases := map[string][]byte{
		"empty":          {},
		"tiny":           {0x00},
		"text":           []byte("this is not a video file, it is a note about one"),
		"ebml then junk": append([]byte{0x1A, 0x45, 0xDF, 0xA3}, bytes.Repeat([]byte{0xFF}, 64)...),
		"ebml then zeros": append([]byte{0x1A, 0x45, 0xDF, 0xA3},
			bytes.Repeat([]byte{0x00}, 512)...),
		"ftyp then junk": append([]byte("\x00\x00\x00\x18ftypisom"),
			bytes.Repeat([]byte{0xAA}, 200)...),
		"ftyp then huge box": append([]byte("\x00\x00\x00\x10ftypisom\x00\x00\x02\x00"),
			[]byte("\x7f\xff\xff\xffmoov")...),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			info, err := mediainfo.Probe(bytes.NewReader(in), int64(len(in)))
			if err == nil {
				t.Fatalf("Probe(%s) = %+v, want an error", name, info)
			}
			if info.Measured() {
				t.Errorf("Probe(%s) claimed measurements: %+v", name, info.Video)
			}
		})
	}
}

// TestProbeTruncatedPrefixes feeds every fixture at every truncation point a
// partial download or a flaky mount could produce. Nothing here may panic, and
// nothing may report a resolution it did not read.
func TestProbeTruncatedPrefixes(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) == ".txt" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(e.Name(), func(t *testing.T) {
			for n := 0; n < len(data); n += 97 { // a prime stride: no alignment luck
				prefix := data[:n]
				info, _ := mediainfo.Probe(bytes.NewReader(prefix), int64(len(prefix)))
				if info.Video != nil && (info.Video.Width < 0 || info.Video.Height < 0) {
					t.Fatalf("negative dimensions at %d bytes: %+v", n, info.Video)
				}
			}
		})
	}
}

// TestProbeCorruptedBytes flips bytes throughout a real header. Every result is
// acceptable except a panic — this is the failure mode that would take down a
// scan of somebody's library.
func TestProbeCorruptedBytes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "mkv-1080p-h264-ac3.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4096 && i < len(data); i += 7 {
		mutated := append([]byte(nil), data...)
		mutated[i] ^= 0xFF
		_, _ = mediainfo.Probe(bytes.NewReader(mutated), int64(len(mutated)))
	}
	mp4Data, err := os.ReadFile(filepath.Join("testdata", "mp4-1080p-h264-aac-faststart.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4096 && i < len(mp4Data); i += 7 {
		mutated := append([]byte(nil), mp4Data...)
		mutated[i] ^= 0xFF
		_, _ = mediainfo.Probe(bytes.NewReader(mutated), int64(len(mutated)))
	}
}

// TestProbeBudget: a file whose headers are buried past the byte cap must give
// up rather than read on. The cap is the promise that a probe sweep over a
// multi-TB library stays cheap, so it needs a test that would notice its loss.
func TestProbeBudget(t *testing.T) {
	// An MKV whose EBML header claims a body far past the MKV budget: the
	// walker must refuse rather than seek into the void and keep reading.
	var buf bytes.Buffer
	buf.Write([]byte{0x1A, 0x45, 0xDF, 0xA3})
	buf.Write([]byte{0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40}) // 8-byte size
	buf.Write(bytes.Repeat([]byte{0x00}, 64))

	counting := &countingReaderAt{r: bytes.NewReader(buf.Bytes())}
	_, err := mediainfo.Probe(counting, 1<<40) // claim a 1 TiB file
	if err == nil {
		t.Fatal("Probe over a nonsense header succeeded")
	}
	if counting.bytes > 8<<20 {
		t.Errorf("read %d bytes chasing a bad header; the budget is 8 MiB", counting.bytes)
	}
}

type countingReaderAt struct {
	r     *bytes.Reader
	bytes int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.bytes += int64(n)
	return n, err
}

// TestProbeNilAndZero covers the degenerate calls the app layer can make when a
// file vanished between the scan and the probe.
func TestProbeNilAndZero(t *testing.T) {
	if _, err := mediainfo.Probe(nil, 100); err == nil {
		t.Error("Probe(nil) succeeded")
	}
	if _, err := mediainfo.Probe(bytes.NewReader([]byte("x")), 0); err == nil {
		t.Error("Probe(size=0) succeeded")
	}
}

func TestDisplayNames(t *testing.T) {
	cases := map[string]string{
		"hevc": "HEVC", "h264": "H.264", "truehd": "TrueHD", "eac3": "E-AC-3",
		"hdr10": "HDR10", "dv": "Dolby Vision", "somethingnew": "SOMETHINGNEW",
	}
	for in, want := range cases {
		if got := mediainfo.Display(in); got != want {
			t.Errorf("Display(%q) = %q, want %q", in, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Minimal container builders. These exist so the tests above can state a format
// fact ("a dvcC BlockAddIDType means Dolby Vision") in the bytes that carry it,
// without needing an encoder that can produce the feature.
// ---------------------------------------------------------------------------

// ebml writes one element: ID bytes as they appear on the wire, then a
// minimally-encoded size, then the payload.
func ebml(id uint32, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := idBytes(id)
	out = append(out, sizeVint(uint64(len(body)))...)
	return append(out, body...)
}

// idBytes renders an element ID, whose marker bit already tells us its width.
func idBytes(id uint32) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// sizeVint encodes a length in the shortest EBML form that fits it.
func sizeVint(n uint64) []byte {
	for length := 1; length <= 8; length++ {
		limit := uint64(1)<<(7*length) - 1
		if n < limit {
			out := make([]byte, length)
			v := n | uint64(1)<<(7*length)
			for i := length - 1; i >= 0; i-- {
				out[i] = byte(v)
				v >>= 8
			}
			return out
		}
	}
	return []byte{0xFF}
}

// be renders an unsigned integer the way EBML stores them: big-endian, minimal.
func be(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var out []byte
	started := false
	for shift := 56; shift >= 0; shift -= 8 {
		b := byte(v >> shift)
		if b != 0 {
			started = true
		}
		if started {
			out = append(out, b)
		}
	}
	return out
}

func f64(v float64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, math.Float64bits(v))
	return out
}

func buildMatroska(children ...[]byte) []byte {
	header := ebml(0x1A45DFA3, ebml(0x4286, be(1)), ebml(0x4282, []byte("matroska")))
	return append(header, ebml(0x18538067, children...)...)
}

// box writes an ISO base media box: size, type, payload.
func box(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], typ)
	return append(out, body...)
}

func buildMP4(children ...[]byte) []byte {
	ftyp := box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2avc1mp41"))
	return append(ftyp, box("moov", children...)...)
}

func mvhd(timescale, duration uint32) []byte {
	body := make([]byte, 100)
	// version 0 + flags, then creation(4) modification(4) timescale(4) duration(4)
	binary.BigEndian.PutUint32(body[12:], timescale)
	binary.BigEndian.PutUint32(body[16:], duration)
	return box("mvhd", body)
}

func trak(handler string, stbl ...[]byte) []byte {
	hdlrBody := make([]byte, 24)
	copy(hdlrBody[8:], handler)
	return box("trak",
		box("mdia",
			box("hdlr", hdlrBody),
			box("minf", box("stbl", stbl...)),
		),
	)
}

func stsd(entries ...[]byte) []byte {
	head := make([]byte, 8)
	binary.BigEndian.PutUint32(head[4:], uint32(len(entries)))
	var body []byte
	for _, e := range entries {
		body = append(body, e...)
	}
	return append(head, body...)
}

// visualEntry builds a VisualSampleEntry with the given format, dimensions and
// trailing sub-boxes. The 78-byte preamble layout is documented in mp4.go.
func visualEntry(format string, w, h int, subBoxes ...[]byte) []byte {
	body := make([]byte, 78)
	binary.BigEndian.PutUint16(body[24:], uint16(w))
	binary.BigEndian.PutUint16(body[26:], uint16(h))
	return box(format, append(body, flatten(subBoxes)...))
}

func flatten(in [][]byte) []byte {
	var out []byte
	for _, b := range in {
		out = append(out, b...)
	}
	return out
}
