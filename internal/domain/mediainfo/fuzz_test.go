package mediainfo_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
)

// FuzzProbe is the standing guarantee behind ADR 0013 §2: this package reads
// arbitrary bytes off a stranger's disk, on a background job, inside the same
// process that serves the UI. A panic here is a crashed scan for everyone.
//
// The invariants are deliberately weak — Probe is allowed to fail on anything —
// but they are the ones a corrupt file could otherwise violate: no panic, no
// negative or absurd dimensions, and no measurement claimed without a video
// track to have measured.
func FuzzProbe(f *testing.F) {
	seedFromFixtures(f)
	f.Add([]byte{})
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3})
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte("\x00\x00\x00\x10ftypisom\x00\x00\x02\x00"))
	f.Add([]byte("\x00\x00\x00\x01ftypisom\xff\xff\xff\xff\xff\xff\xff\xff"))
	// An MKV element whose declared size is larger than any disk.
	f.Add([]byte{
		0x1A, 0x45, 0xDF, 0xA3, 0x84, 0x00, 0x00, 0x00, 0x00,
		0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := mediainfo.Probe(bytes.NewReader(data), int64(len(data)))
		if err != nil && info.Measured() {
			// Partial results are fine, but "measured" must mean measured.
			if info.Video.Width <= 0 || info.Video.Height <= 0 {
				t.Fatalf("Measured() true with dimensions %dx%d", info.Video.Width, info.Video.Height)
			}
		}
		if info.Video != nil {
			if info.Video.Width < 0 || info.Video.Height < 0 {
				t.Fatalf("negative dimensions: %+v", info.Video)
			}
			if info.Video.BitDepth < 0 || info.Video.BitDepth > 16 {
				t.Fatalf("implausible bit depth %d", info.Video.BitDepth)
			}
		}
		if info.DurationMS < 0 {
			t.Fatalf("negative duration %d", info.DurationMS)
		}
		if info.BitrateKbps < 0 {
			t.Fatalf("negative bitrate %d", info.BitrateKbps)
		}
		if tier := info.ResolutionTier(); tier != 0 && tier != 480 && tier != 720 &&
			tier != 1080 && tier != 2160 {
			t.Fatalf("tier %d is not one of the resolution vocabulary", tier)
		}
		// Summary must never panic and must be empty when nothing was measured.
		if s := info.Summary(); s != "" && !info.Measured() {
			t.Fatalf("Summary %q rendered without a measurement", s)
		}
	})
}

// seedFromFixtures gives the fuzzer real headers to mutate — far more likely to
// reach the deep paths than random bytes ever are.
func seedFromFixtures(f *testing.F) {
	f.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) == ".txt" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			continue
		}
		// Seed with the first 8 KiB: enough to carry the headers, small enough
		// that the fuzzer's mutations land somewhere interesting.
		if len(data) > 8192 {
			data = data[:8192]
		}
		f.Add(data)
	}
}
