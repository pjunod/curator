package dovi

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParseRPU holds the package to the mediainfo rule: arbitrary bytes from a
// stranger's file never panic, and whatever parses stays inside the
// vocabulary.
func FuzzParseRPU(f *testing.F) {
	seeds, _ := filepath.Glob(filepath.Join("testdata", "*"))
	for _, p := range seeds {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b)
		}
	}
	f.Add(buildRPUSeed(true))
	f.Add(buildRPUSeed(false))
	f.Add([]byte{0x7C, 0x01, 0x19})
	f.Add([]byte{0x19, 0x08, 0x09, 0x80})

	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseRPU(b)
		if err != nil {
			return
		}
		switch r.ELType {
		case "", ELNone, ELMEL, ELFEL:
		default:
			t.Fatalf("ELType %q outside the vocabulary", r.ELType)
		}
		switch r.CMVersion {
		case "", CM29, CM40:
		default:
			t.Fatalf("CMVersion %q outside the vocabulary", r.CMVersion)
		}
		if r.Profile < 0 || r.Profile > 8 {
			t.Fatalf("profile %d", r.Profile)
		}
	})
}

func FuzzIsHDR10Plus(f *testing.F) {
	f.Add([]byte{NALPrefixSEI << 1, 0x01, 4, 8, 0xB5, 0x00, 0x3C, 0x00, 0x01, 0x04, 0x01, 0x40, 0x80})
	f.Add([]byte{NALPrefixSEI << 1, 0x01, 0xFF, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = IsHDR10Plus(b)
		_, _ = ParseConfig(b)
	})
}

// buildRPUSeed is buildRPU without a *testing.T, for the fuzz corpus.
func buildRPUSeed(mel bool) []byte {
	return buildRPU(&testing.T{}, mel)
}
