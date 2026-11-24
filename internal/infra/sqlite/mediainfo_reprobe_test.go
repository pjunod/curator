package sqlite

import (
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
)

// A measurement taken by an older prober is re-taken once the prober learns
// to read more (ADR 0026: Dolby Vision and HDR10+ reached files probed before
// it). Only measured records qualify; the user's manual quality is left alone.
func TestNeedsProbeAfterProberRevision(t *testing.T) {
	const size = 1 << 30
	rec := func(p mediainfo.Provenance, rev int) FileQuality {
		return FileQuality{Probed: true, Size: size, Provenance: p,
			Info: mediainfo.Info{Container: mediainfo.ContainerMKV, ProberRevision: rev,
				Video: &mediainfo.VideoInfo{Codec: "hevc", Width: 3840, Height: 2160}}}
	}
	h264 := rec(mediainfo.ProvenanceProbe, 1)
	h264.Info.Video = &mediainfo.VideoInfo{Codec: "h264", Width: 1920, Height: 1080}
	cases := []struct {
		name string
		f    FileQuality
		want bool
	}{
		{"current measurement", rec(mediainfo.ProvenanceProbe, mediainfo.ProberRevision), false},
		{"older measurement", rec(mediainfo.ProvenanceProbe, mediainfo.ProberRevision-1), true},
		{"pre-revision measurement", rec(mediainfo.ProvenanceProbe, 0), true},
		{"older implausible measurement", rec(mediainfo.ProvenanceImplausible, 0), true},
		{"manual quality", rec(mediainfo.ProvenanceManual, 0), false},
		// Revision 2 adds facts only for HEVC; an H.264 file is not re-read.
		{"older H.264 measurement", h264, false},
	}
	for _, tc := range cases {
		if got := tc.f.NeedsProbe(size); got != tc.want {
			t.Errorf("%s: NeedsProbe = %v, want %v", tc.name, got, tc.want)
		}
	}
}
