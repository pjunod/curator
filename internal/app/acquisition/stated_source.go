package acquisition

import (
	"context"

	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/probe"
)

// StatedSources are the video sources a person may state for files they
// import by hand — a disc they ripped carries no release name to claim one.
// Resolution is never stated: it is measured (ADR 0013).
var StatedSources = []quality.Source{
	quality.SourceRemux, quality.SourceBluray, quality.SourceWEBDL,
	quality.SourceWEBRip, quality.SourceHDTV, quality.SourceDVD,
}

// ValidStatedSource reports whether s is one of StatedSources (or empty).
func ValidStatedSource(s string) bool {
	if s == "" {
		return true
	}
	for _, v := range StatedSources {
		if string(v) == s {
			return true
		}
	}
	return false
}

// handMadeQuality is the quality a manual import names and judges a video
// file by before placement measures it: the name's claim, the person's
// stated source over it, and — when nothing named a resolution, as with a
// rip — the resolution read from the file's header. The plan and the import
// use it alike, so the confirm step shows the name the import will write.
func handMadeQuality(src string, named quality.Quality, stated quality.Source) quality.Quality {
	q := named
	if stated != "" {
		q.Source = stated
	}
	if q.Resolution == 0 {
		if info, _ := probe.File(src); info.Measured() {
			q.Resolution = info.ResolutionTier()
		}
	}
	return q
}

type statedSourceKey struct{}

func withStatedSource(ctx context.Context, src quality.Source) context.Context {
	if src == "" {
		return ctx
	}
	return context.WithValue(ctx, statedSourceKey{}, src)
}

// statedSource is the source a person stated for this import, if any. It is
// their word about the files (ProvenanceManual): it outranks the name and the
// release, and only a measurement proving the file is not what anything
// claims (implausible) sets it aside.
func statedSource(ctx context.Context) (quality.Source, bool) {
	src, ok := ctx.Value(statedSourceKey{}).(quality.Source)
	return src, ok && src != ""
}
