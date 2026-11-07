package ports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Pipeline contract v2 (docs/PIPELINE_CONTRACT.md section 3.2): every
// observation of a Runner job, from the event stream or from the poll, is one
// RunnerFact, and one reducer applies them all the same way.

// FactKind names what a RunnerFact says happened.
type FactKind string

const (
	// FactAdmitted binds a job handle to a submitting row.
	FactAdmitted FactKind = "admitted"
	// FactStatus is progress, a stage, or a hold: never a decision.
	FactStatus FactKind = "status"
	// FactTerminal is the job's one terminal outcome, authored by Runner.
	FactTerminal FactKind = "terminal"
	// FactPublicationReady is a salvage publication of a failed job.
	FactPublicationReady FactKind = "publication_ready"
	// FactGone is Curator giving up on a job Runner no longer knows. It is
	// provisional: any later Runner fact re-opens it.
	FactGone FactKind = "gone"
)

// Terminal outcomes on the wire.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"
)

// Failure reasons (contract section 2.2), plus the two Curator authors
// provisionally (submit, lost) and the one migrated parks carry.
const (
	ReasonHealth      = "health"
	ReasonFetch       = "fetch"
	ReasonPar         = "par"
	ReasonUnpack      = "unpack"
	ReasonScript      = "script"
	ReasonPublish     = "publish"
	ReasonIO          = "io"
	ReasonIdentity    = "identity"
	ReasonHoldExpired = "hold_expired"
	ReasonSubmit      = "submit"
	ReasonLost        = "lost"
	ReasonParked      = "parked"
)

// Fetch causes: whether an exhausted fetch is the item's fault.
const (
	FetchCauseGone               = "gone"
	FetchCauseIndexerUnavailable = "indexer_unavailable"
)

// RunnerFact is one observation about one job, from any feed.
type RunnerFact struct {
	Kind FactKind
	// Transfer must equal the row's token or the fact is dropped. Empty only
	// on a legacy event that carried none; the reducer then falls back to the
	// handle.
	Transfer    string
	Handle      Handle
	Status      *DownloadStatus
	Terminal    *RunnerTerminal
	Publication *RunnerPublication
	// Source names the feed for the handoff trace: "event <seq>",
	// "history <incarnation>:<seq>", "deliveries <seq>", "admission", "poll".
	Source             string
	HistorySeq         int64
	HistoryIncarnation string
}

// Fetched is what Runner measured it downloaded for the job.
type Fetched struct {
	SuccessArticles int64 `json:"success_articles"`
	TotalArticles   int64 `json:"total_articles"`
	Bytes           int64 `json:"bytes"`
}

// Known reports whether Runner said anything about what it fetched.
func (f Fetched) Known() bool { return f.Bytes > 0 || f.TotalArticles > 0 }

// RunnerTerminal is the body of a terminal fact.
type RunnerTerminal struct {
	Outcome       string
	Reason        string
	FetchCause    string
	Stage         string
	Message       string
	BlamesRelease bool
	Salvage       string // none | pending
	KeepOnFailure bool
	Fetched       Fetched
	Publication   *RunnerPublication
	// The legacy history strings, kept for one release (contract section 6).
	LegacyPPStatus, LegacyFinalDir string
}

// RunnerPublication names one published generation.
type RunnerPublication struct {
	Path           string
	Generation     string
	ManifestDigest string
	Files          int
	CrossVolume    bool
	Kind           string // completed | salvage
	// Manifest is attached when the feed carried the manifest body (the v1
	// delivery record); nil when only the event's summary is known.
	Manifest *Manifest
}

// Manifest is an immutable file list with sizes and digests.
type Manifest struct {
	Generation  string         `json:"generation"`
	Files       []ManifestFile `json:"files"`
	Digest      string         `json:"digest"`
	CrossVolume bool           `json:"cross_volume,omitempty"`
	// Source is where the manifest came from: file (.runner-manifest.json)
	// or v1_record (Runner's v1 delivery record).
	Source string `json:"-"`
	// DeliveryID is the v1 record's id, for a later lookup.
	DeliveryID string `json:"delivery_id,omitempty"`
}

// ManifestFile is one file in a manifest.
type ManifestFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// ManifestFileName is the commit marker of a publication directory.
const ManifestFileName = ".runner-manifest.json"

// ManifestDigest is the deployed runner-manifest-v1 algorithm, shared with
// Runner (nzbd-state/src/artifacts/delivery.rs): SHA-256 over
// "runner-manifest-v1\n" then, per file in bytewise path order,
// "<len(path)>:<path>\n<bytes>\n<sha256>\n"; bare lowercase hex. The
// publications already on disk depend on it never changing.
func ManifestDigest(files []ManifestFile) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("runner delivery manifest is empty")
	}
	ordered := append([]ManifestFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return bytes.Compare([]byte(ordered[i].Path), []byte(ordered[j].Path)) < 0 })
	h := sha256.New()
	_, _ = h.Write([]byte("runner-manifest-v1\n"))
	for _, f := range ordered {
		if !utf8.ValidString(f.Path) {
			return "", fmt.Errorf("runner manifest path is not valid UTF-8")
		}
		_, _ = io.WriteString(h, strconv.Itoa(len([]byte(f.Path)))+":")
		_, _ = h.Write([]byte(f.Path))
		_, _ = h.Write([]byte("\n"))
		_, _ = io.WriteString(h, strconv.FormatInt(f.Bytes, 10)+"\n"+f.SHA256+"\n")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ManifestFromRunnerFiles converts a v1 delivery record's file list.
func ManifestFromRunnerFiles(generation, digest, deliveryID string, files []RunnerFile) Manifest {
	m := Manifest{Generation: generation, Digest: digest, Source: "v1_record", DeliveryID: deliveryID}
	for _, f := range files {
		m.Files = append(m.Files, ManifestFile{Path: f.Path, Bytes: f.SizeBytes, SHA256: f.SHA256})
	}
	return m
}

// HistoryCursor is the ordered history reader's position (contract 2.5).
type HistoryCursor struct {
	Incarnation    string
	SinceSeq       int64
	HeadSeq        int64
	RetainedMinSeq int64
}

// HistoryReader reads terminal facts from Runner's history in seq order.
// continuityLost is true when the stored incarnation differs or the cursor
// fell below what Runner retains; every outstanding transfer must then be
// reconciled individually before the cursor is trusted again.
type HistoryReader interface {
	HistorySince(ctx context.Context, c HistoryCursor, limit int) (facts []RunnerFact, next HistoryCursor, continuityLost bool, err error)
}

// DeliveriesReader pages Runner's publication feed (accepted rows included)
// into terminal{completed} and publication_ready facts.
type DeliveriesReader interface {
	DeliveriesSince(ctx context.Context, after uint64, limit int) ([]RunnerFact, uint64, error)
}

// RunnerFeeds is the capability that puts a client on the pipeline v2
// reducer: both reconciliation feeds plus the admission lookup.
type RunnerFeeds interface {
	HistoryReader
	DeliveriesReader
	RequestAdmissionProtocol
}

// LegacyTerminal maps a legacy history status string (an old Runner, or a
// receipt without typed fields) to a terminal: SUCCESS* completed, DELETED*
// cancelled, everything else failed with the reason its string names.
// blamesRelease is today's rule: a failure blames the release unless the
// caller knows it is blameless.
func LegacyTerminal(status string, blameless bool) RunnerTerminal {
	switch {
	case strings.HasPrefix(status, "SUCCESS"):
		return RunnerTerminal{Outcome: OutcomeCompleted, Salvage: "none", LegacyPPStatus: status}
	case strings.HasPrefix(status, "DELETED"):
		return RunnerTerminal{Outcome: OutcomeCancelled, Salvage: "none", LegacyPPStatus: status, Message: "removed in Runner"}
	}
	t := RunnerTerminal{Outcome: OutcomeFailed, Salvage: "none", BlamesRelease: !blameless, LegacyPPStatus: status, Message: status}
	switch {
	case strings.HasPrefix(status, "FAILURE/HEALTH"):
		t.Reason, t.Stage = ReasonHealth, "health"
	case strings.HasPrefix(status, "FAILURE/FETCH"):
		t.Reason, t.Stage = ReasonFetch, "fetch"
	case strings.HasPrefix(status, "PAR_FAILURE"):
		t.Reason, t.Stage = ReasonPar, "par_repair"
	case strings.HasPrefix(status, "UNPACK_FAILURE"):
		t.Reason, t.Stage = ReasonUnpack, "unpack"
	case strings.HasPrefix(status, "SCRIPT_FAILURE"):
		t.Reason, t.Stage = ReasonScript, "script"
	default:
		t.Reason = ReasonIO
	}
	return t
}
