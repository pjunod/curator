package mediainfo

// Provenance says where a file's recorded quality came from. It exists because
// "1080p WEB-DL" means different things depending on who said it: the pixels,
// a release group's filename, or a tracker listing nobody verified. ADR 0013 §3
// makes that distinction storable, showable, and — for the don't-churn rule —
// actionable.
type Provenance string

// The provenance vocabulary. These strings are persisted in
// media_files.quality_provenance; extend, never renumber.
const (
	// ProvenanceUnknown is a row written before ADR 0013 shipped. Nothing but
	// the filename parser could have written those, so it reads as Filename
	// wherever a decision needs a value.
	ProvenanceUnknown Provenance = ""
	// ProvenanceProbe: the bytes were measured and the measurement won.
	ProvenanceProbe Provenance = "probe"
	// ProvenanceFilename: measurement was unavailable or inconclusive on the
	// source axis, and an uncontradicted filename token supplied it.
	ProvenanceFilename Provenance = "filename"
	// ProvenanceRelease: the grabbed release's own claimed quality supplied
	// the source, at import time, when nothing better contradicted it.
	ProvenanceRelease Provenance = "release"
	// ProvenanceManual: a human said so. Nothing overrides this.
	ProvenanceManual Provenance = "manual"
	// ProvenanceFailed: the file exists and could not be read or parsed. This
	// is the one state that means "on disk, quality unverified" — distinct
	// from missing everywhere a decision is made (ADR 0013 §5).
	ProvenanceFailed Provenance = "failed"
	// ProvenanceImplausible: the file was read, and what it says about itself
	// cannot be true (see plausible.go). This is emphatically NOT Failed.
	// Failed means we do not know, and not knowing is a reason to leave a file
	// alone. This means we DO know, and what we know is that the file is not
	// what it claims — which is a reason to keep hunting, because the thing
	// being protected from replacement turns out not to exist.
	ProvenanceImplausible Provenance = "implausible"
)

// Confidence qualifies an INFERRED source. Resolution is never inferred once a
// probe succeeds, so this never applies to it.
type Confidence string

// The confidence vocabulary.
const (
	ConfidenceNone   Confidence = ""
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Measured reports whether this provenance means the pixels were read.
func (p Provenance) Measured() bool { return p == ProvenanceProbe }

// Verified reports whether the SOURCE axis of the recorded quality can be
// trusted enough to justify replacing a file that already meets the target
// resolution. This is the don't-churn predicate (ADR 0013 §5): a guess about
// WEB-DL versus Bluray must never evict a file that is already the right size.
//
// A human's word counts. A high-confidence measurement counts. An
// uncontradicted filename token counts — it is a claim, but it is a claim
// somebody made about this specific file, and the measurements did not
// disagree with it. A medium or low inference does not.
func (p Provenance) Verified(c Confidence) bool {
	switch p {
	case ProvenanceImplausible:
		// Not "unverified" in the cautious sense — actively disbelieved. The
		// don't-churn rule protects files we might be wrong about; there is
		// nothing to be wrong about here.
		return false
	case ProvenanceManual, ProvenanceFilename, ProvenanceRelease:
		return true
	case ProvenanceProbe:
		return c == ConfidenceHigh
	default:
		return false
	}
}

// Label renders the provenance as the UI badge text.
func (p Provenance) Label() string {
	switch p {
	case ProvenanceProbe:
		return "measured"
	case ProvenanceFilename:
		return "from filename"
	case ProvenanceRelease:
		return "from release"
	case ProvenanceManual:
		return "set by hand"
	case ProvenanceFailed:
		return "unreadable"
	case ProvenanceImplausible:
		return "does not add up"
	default:
		return "unverified"
	}
}

// Valid reports whether p is a value this codebase writes. Storage uses it to
// refuse to persist a typo as a provenance.
func (p Provenance) Valid() bool {
	switch p {
	case ProvenanceUnknown, ProvenanceProbe, ProvenanceFilename,
		ProvenanceRelease, ProvenanceManual, ProvenanceFailed,
		ProvenanceImplausible:
		return true
	}
	return false
}

// Valid reports whether c is a confidence this codebase writes.
func (c Confidence) Valid() bool {
	switch c {
	case ConfidenceNone, ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		return true
	}
	return false
}
