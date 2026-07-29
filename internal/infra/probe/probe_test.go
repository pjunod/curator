package probe_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/infra/probe"
)

// corpusFile borrows one of the mediainfo package's generated container
// fixtures. They are the only real headers in the tree, and probe's whole job
// is handing a real file to that parser — a fixture of our own would either
// duplicate them or test a parser we do not own against bytes it has never
// seen.
func corpusFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "domain", "mediainfo", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("mediainfo corpus fixture unavailable: %v", err)
	}
	return path
}

// noExternalProber points MONARR_FFPROBE at a binary that does not exist, so
// a test of the native path cannot accidentally pick up an ffprobe that
// happens to be installed on the machine running the suite. monarr's own
// image is distroless and has none.
func noExternalProber(t *testing.T) {
	t.Helper()
	t.Setenv(probe.FFprobeEnv, filepath.Join(t.TempDir(), "definitely-not-here"))
}

// The happy path: a real container header, opened and measured. Everything
// else in this file is about what happens when that does not work.
func TestTailFileMeasuresARealContainer(t *testing.T) {
	noExternalProber(t)
	info, err := probe.File(corpusFile(t, "mkv-1080p-h264-ac3.mkv"))
	if err != nil {
		t.Fatalf("probing a valid mkv: %v", err)
	}
	if info.Container != mediainfo.ContainerMKV {
		t.Errorf("container = %q, want mkv", info.Container)
	}
	if info.Video == nil || info.Video.Width != 1920 || info.Video.Height != 1080 {
		t.Errorf("video = %+v, want 1920x1080", info.Video)
	}
}

// A path that is not there is an ordinary error, not a partially-filled
// record: nothing was learned, so nothing should be recorded.
func TestTailFileReportsAMissingPath(t *testing.T) {
	noExternalProber(t)
	info, err := probe.File(filepath.Join(t.TempDir(), "gone.mkv"))
	if err == nil {
		t.Fatal("probing a missing file succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want a not-exist error the caller can classify", err)
	}
	if info.Container != "" {
		t.Errorf("a missing file produced a record: %+v", info)
	}
}

// A directory opens fine and stats fine, so without an explicit check it
// would reach the parser and come back as an unreadable container — a
// misleading answer to "what is this file".
func TestTailFileRefusesADirectory(t *testing.T) {
	noExternalProber(t)
	dir := t.TempDir()
	_, err := probe.File(dir)
	if err == nil {
		t.Fatal("probing a directory succeeded")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("err = %v, want it to say the path is a directory", err)
	}
}

// The distinction ADR 0013 §5 turns on: an .mkv that will not sniff is
// TRUNCATED, corrupt, still downloading, or behind a permission we did not
// have. Every one of those gets fixed, so the container is left BLANK — a
// failure worth retrying — rather than marked unsupported forever.
func TestTailAnUnreadableNativeContainerIsAFailureNotAVerdict(t *testing.T) {
	noExternalProber(t)
	path := filepath.Join(t.TempDir(), "still-downloading.mkv")
	if err := os.WriteFile(path, []byte("not an EBML header at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := probe.File(path)
	if !errors.Is(err, mediainfo.ErrUnknownContainer) {
		t.Fatalf("err = %v, want ErrUnknownContainer", err)
	}
	if info.Container != "" {
		t.Errorf("container = %q — marking a broken .mkv unsupported is what made "+
			"'the probe failed once' permanent", info.Container)
	}
}

// The other half of that distinction: an .avi was never going to parse here,
// so the marker is recorded and scans stop re-reading it forever.
func TestTailANonNativeContainerIsMarkedUnsupported(t *testing.T) {
	noExternalProber(t)
	path := filepath.Join(t.TempDir(), "broadcast.avi")
	if err := os.WriteFile(path, []byte("RIFF????AVI LIST"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := probe.File(path)
	if !errors.Is(err, mediainfo.ErrUnknownContainer) {
		t.Fatalf("err = %v, want ErrUnknownContainer", err)
	}
	if info.Container != mediainfo.Unsupported(".avi") {
		t.Errorf("container = %q, want %q", info.Container, mediainfo.Unsupported(".avi"))
	}
	if !mediainfo.IsUnsupported(info.Container) {
		t.Error("the marker does not read back as unsupported")
	}
}

// When an external ffprobe IS configured, an unsupported container gets
// enriched from it — and comes back with no error, because the file was
// measured after all. The marker stays so the UI can still say where the
// numbers came from.
func TestTailAConfiguredFFprobeEnrichesAnUnsupportedContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub prober is a shell script")
	}
	stub := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\ncat <<'EOF'\n" + `{"streams":[
		{"codec_type":"video","codec_name":"mpeg2video","width":720,"height":576,
		 "bits_per_raw_sample":"8","field_order":"tt"},
		{"codec_type":"audio","codec_name":"ac3","channels":6}],
	 "format":{"duration":"60.0"}}` + "\nEOF\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(probe.FFprobeEnv, stub)

	path := filepath.Join(t.TempDir(), "broadcast.avi")
	if err := os.WriteFile(path, []byte("RIFF????AVI LIST"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := probe.File(path)
	if err != nil {
		t.Fatalf("an enriched probe should not report an error: %v", err)
	}
	if info.Container != mediainfo.Unsupported(".avi") {
		t.Errorf("container = %q — the enrichment overwrote the marker saying "+
			"which prober produced this", info.Container)
	}
	if info.Video == nil || info.Video.Codec != "mpeg2" || !info.Video.Interlaced {
		t.Errorf("video = %+v, want interlaced mpeg2 from the external prober", info.Video)
	}
}

// A configured ffprobe that fails leaves the record exactly as the native
// path left it: marker set, error preserved. Opportunistic enrichment must
// never turn into a new way to lose the original answer.
func TestTailAFailingFFprobeChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub prober is a shell script")
	}
	stub := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(probe.FFprobeEnv, stub)

	path := filepath.Join(t.TempDir(), "broadcast.avi")
	if err := os.WriteFile(path, []byte("RIFF????AVI LIST"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := probe.File(path)
	if !errors.Is(err, mediainfo.ErrUnknownContainer) {
		t.Fatalf("err = %v, want the native error preserved", err)
	}
	if info.Container != mediainfo.Unsupported(".avi") {
		t.Errorf("container = %q, want the unsupported marker", info.Container)
	}
}

// The two probers must not record different vocabularies for the same file,
// which is the whole reason ParseFFprobe is exported and pinned here rather
// than left to whichever ffmpeg build the test machine happens to have.
func TestTailParseFFprobeSpeaksTheNativeVocabulary(t *testing.T) {
	raw := []byte(`{"streams":[
		{"codec_type":"video","codec_name":"hevc","width":3840,"height":2160,
		 "bits_per_raw_sample":"10","color_transfer":"smpte2084","field_order":"progressive"},
		{"codec_type":"audio","codec_name":"truehd","channels":8},
		{"codec_type":"audio","codec_name":"dca","channels":6},
		{"codec_type":"subtitle","codec_name":"subrip"}],
	 "format":{"duration":"7200.5"}}`)

	// 8 GB over two hours: the bitrate is derived, not read.
	info, err := probe.ParseFFprobe(raw, 8<<30)
	if err != nil {
		t.Fatal(err)
	}
	if info.Video.Codec != "hevc" || info.Video.BitDepth != 10 || info.Video.Interlaced {
		t.Errorf("video = %+v", info.Video)
	}
	if len(info.Video.HDR) != 1 || info.Video.HDR[0] != mediainfo.HDR10 {
		t.Errorf("hdr = %v, want hdr10 from the smpte2084 transfer", info.Video.HDR)
	}
	if len(info.Audio) != 2 {
		t.Fatalf("audio tracks = %d, want 2 (the subtitle stream is not audio)", len(info.Audio))
	}
	// ffmpeg's "dca" is monarr's "dts"; recording both spellings would mean
	// two vocabularies for one pipeline.
	if info.Audio[0].Codec != "truehd" || info.Audio[1].Codec != "dts" {
		t.Errorf("audio = %+v", info.Audio)
	}
	if info.DurationMS != 7200500 {
		t.Errorf("duration = %d ms, want 7200500", info.DurationMS)
	}
	if info.BitrateKbps == 0 {
		t.Error("bitrate was not derived from size and duration")
	}
}

// ffmpeg has several names for codecs monarr records under one. The mapping
// is what keeps the external and native probers from filling the same column
// with two vocabularies, which would break every quality comparison that
// reads it.
func TestTailParseFFprobeNormalizesFFmpegCodecNames(t *testing.T) {
	cases := map[string]string{
		"avc": "h264", "H264": "h264", "hevc": "hevc", "mpeg2video": "mpeg2",
		"vc1": "vc1", "vp9": "vp9",
	}
	for in, want := range cases {
		info, err := probe.ParseFFprobe([]byte(`{"streams":[
			{"codec_type":"video","codec_name":"`+in+`"}]}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		if info.Video.Codec != want {
			t.Errorf("video codec %q normalized to %q, want %q", in, info.Video.Codec, want)
		}
	}
	audio := map[string]string{
		"mlp": "truehd", "truehd": "truehd", "dca": "dts", "dts": "dts",
		"pcm_s24le": "pcm", "pcm_s16be": "pcm", "EAC3": "eac3",
	}
	for in, want := range audio {
		info, err := probe.ParseFFprobe([]byte(`{"streams":[
			{"codec_type":"video","codec_name":"h264"},
			{"codec_type":"audio","codec_name":"`+in+`","channels":2}]}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		if info.Audio[0].Codec != want {
			t.Errorf("audio codec %q normalized to %q, want %q", in, info.Audio[0].Codec, want)
		}
	}
}

// HLG rides the same transfer field as HDR10, one value over.
func TestTailParseFFprobeReadsHLG(t *testing.T) {
	info, err := probe.ParseFFprobe([]byte(`{"streams":[
		{"codec_type":"video","codec_name":"h265","color_transfer":"arib-std-b67"}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Video.HDR) != 1 || info.Video.HDR[0] != mediainfo.HLG {
		t.Errorf("hdr = %v, want hlg", info.Video.HDR)
	}
	// "h265" and "hevc" are the same codec spelled two ways upstream.
	if info.Video.Codec != "hevc" {
		t.Errorf("codec = %q, want hevc", info.Video.Codec)
	}
}

// First video track wins, exactly as the native walkers do — a file with an
// embedded cover-art "video" stream must not be described by the cover art.
func TestTailParseFFprobeKeepsTheFirstVideoTrack(t *testing.T) {
	info, err := probe.ParseFFprobe([]byte(`{"streams":[
		{"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
		{"codec_type":"video","codec_name":"mjpeg","width":600,"height":900}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.Video.Width != 1920 || info.Video.Codec != "h264" {
		t.Errorf("video = %+v, want the first track", info.Video)
	}
}

// Bit depth defaults to 8 when ffprobe does not say, or says something that
// cannot be a bit depth. Leaving it at 0 would make an 8-bit file look
// unmeasured to everything downstream.
func TestTailParseFFprobeDefaultsBitDepthToEight(t *testing.T) {
	for _, raw := range []string{"", "0", "64", "not-a-number"} {
		info, err := probe.ParseFFprobe([]byte(`{"streams":[
			{"codec_type":"video","codec_name":"h264","bits_per_raw_sample":"`+raw+`"}]}`), 0)
		if err != nil {
			t.Fatal(err)
		}
		if info.Video.BitDepth != 8 {
			t.Errorf("bits_per_raw_sample %q gave bit depth %d, want 8", raw, info.Video.BitDepth)
		}
	}
}

// The error paths: nothing usable came back, and saying so is better than
// recording an empty measurement as fact.
func TestTailParseFFprobeRejectsUnusableOutput(t *testing.T) {
	if _, err := probe.ParseFFprobe([]byte("not json"), 0); err == nil {
		t.Error("unparseable ffprobe output was accepted")
	}
	info, err := probe.ParseFFprobe([]byte(`{"streams":[
		{"codec_type":"audio","codec_name":"flac","channels":2}],"format":{"duration":"bad"}}`), 100)
	if err == nil {
		t.Error("output with no video stream was accepted")
	}
	// The audio it did find is still returned — a partial record beats none.
	if len(info.Audio) != 1 {
		t.Errorf("audio = %+v, want the track that was found kept", info.Audio)
	}
	if info.DurationMS != 0 || info.BitrateKbps != 0 {
		t.Errorf("an unparseable duration produced numbers: %+v", info)
	}
}
