package mediainfo

import "github.com/monarr-media/monarr/internal/domain/quality"

// Resolve turns a measurement plus whatever a name claimed into the one
// quality record monarr stores, and says where each half of it came from.
//
// The asymmetry at the heart of ADR 0013 lives here:
//
//   - RESOLUTION is measured, absolute. A filename token never overrides it,
//     because there is nothing about resolution a name can know that the
//     pixels do not.
//   - SOURCE (WEB-DL vs Bluray vs remux) is not directly measurable. It is
//     inferred from the measurements, cross-checked against the name's claim,
//     and recorded with a confidence — because getting it wrong must not be
//     allowed to evict a file (the don't-churn rule, ADR 0013 §5).
//
// hint is the quality a name claimed: the filename token for a scanned file,
// or the grabbed release's parsed quality at import. Pass the zero value when
// there is no claim. hintProv names which of those it was, so the recorded
// provenance can distinguish "the file was called this" from "the release we
// grabbed was called this".
func Resolve(info Info, hint quality.Quality, hintProv Provenance) (quality.Quality, Provenance, Confidence) {
	if !info.Measured() {
		// Nothing was measured: the claim is all we have, and the row should
		// say so rather than dressing a guess up as a fact.
		if hint.Source != quality.SourceUnknown && hint.Source != "" || hint.Resolution != 0 {
			return normalize(hint), hintProv, ConfidenceNone
		}
		return quality.Quality{Source: quality.SourceUnknown}, ProvenanceFailed, ConfidenceNone
	}

	tier := info.ResolutionTier()

	// Before asking what the file IS, ask whether the measurement can be true
	// at all. A self-refuting measurement supports no verdict, and — the part
	// that actually matters — it must not be allowed to fall through to the
	// name's claim either. The pick order below accepts any token nothing
	// contradicts, so "inconclusive" and "obviously fabricated" would
	// otherwise reach the same place: the filename wins.
	//
	// The resolution tier is still recorded. It is the one field a forged
	// header gains nothing by lying about, and keeping it means the row says
	// "2160p, source unknown, not believed" instead of throwing away the only
	// fact we have.
	if _, bad := Implausible(info); bad {
		return quality.Quality{Source: quality.SourceUnknown, Resolution: tier},
			ProvenanceImplausible, ConfidenceNone
	}

	verdict, confidence := InferSource(info)
	token := hint.Source
	contradicted := Contradicts(info, token)

	// Pick order (plan §4.3). A high-confidence measurement outranks any
	// claim; below that an uncontradicted claim beats a hedged measurement,
	// because somebody actually looked at this file and said so.
	switch {
	case verdict != quality.SourceUnknown && confidence == ConfidenceHigh:
		return quality.Quality{Source: verdict, Resolution: tier}, ProvenanceProbe, confidence
	case token != quality.SourceUnknown && token != "" && !contradicted:
		return quality.Quality{Source: token, Resolution: tier}, hintProv, ConfidenceNone
	case verdict != quality.SourceUnknown:
		return quality.Quality{Source: verdict, Resolution: tier}, ProvenanceProbe, confidence
	default:
		// Resolution is still measured fact even with no source verdict; the
		// row records that, and the source stays honestly unknown.
		return quality.Quality{Source: quality.SourceUnknown, Resolution: tier},
			ProvenanceProbe, ConfidenceNone
	}
}

func normalize(q quality.Quality) quality.Quality {
	if q.Source == "" {
		q.Source = quality.SourceUnknown
	}
	return q
}
