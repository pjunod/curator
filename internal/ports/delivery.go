package ports

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrDeliveryConflict = errors.New("runner delivery acceptance conflicts with its immutable identity")

// RunnerFile is one immutable file in a published delivery manifest.
type RunnerFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

// RunnerFileOutcome records how a source-tree file was handled. The source
// and publication paths plus classification/outcome/reason are immutable;
// source cleanup progress may change after the publication is accepted.
type RunnerFileOutcome struct {
	SourcePath          string  `json:"sourcePath"`
	PublicationPath     *string `json:"publicationPath"`
	Classification      string  `json:"classification"`
	Outcome             string  `json:"outcome"`
	Reason              string  `json:"reason"`
	RepairDependency    bool    `json:"repairDependency,omitempty"`
	SourceCleanupStatus string  `json:"sourceCleanupStatus"`
}

// RunnerDelivery is a durable Runner publication awaiting Curator acceptance.
type RunnerDelivery struct {
	LifecycleTiming       *RunnerLifecycleTiming `json:"lifecycleTiming,omitempty"`
	RunnerStages          *RunnerStages          `json:"runnerStages,omitempty"`
	SourceCopies          []RunnerSourceCopy     `json:"sourceCopies,omitempty"`
	ProtocolVersion       int                    `json:"protocolVersion,omitempty"`
	Sequence              uint64                 `json:"sequence"`
	DeliveryID            string                 `json:"deliveryId"`
	PublicationGeneration string                 `json:"publicationGeneration"`
	SourceArtifactID      string                 `json:"sourceArtifactId,omitempty"`
	SourceGeneration      string                 `json:"sourceGeneration,omitempty"`
	ArtifactID            string                 `json:"artifactId"`
	JobID                 string                 `json:"jobId"`
	RequestToken          string                 `json:"requestToken"`
	RunnerInstanceID      string                 `json:"runnerInstanceId"`
	PublicationPath       string                 `json:"publicationPath"`
	ManifestDigest        string                 `json:"manifestDigest"`
	Files                 []RunnerFile           `json:"files"`
	TerminalOutcome       string                 `json:"terminalOutcome,omitempty"`
	FailureStage          string                 `json:"failureStage,omitempty"`
	FailureCause          string                 `json:"failureCause,omitempty"`
	KeepSourceOnFailure   bool                   `json:"keepSourceOnFailure,omitempty"`
	FileOutcomes          []RunnerFileOutcome    `json:"fileOutcomes,omitempty"`
	ReadyAt               time.Time              `json:"readyAt"`
	SourceCleanup         json.RawMessage        `json:"sourceCleanup,omitempty"`
	// Kind is completed or salvage (Runner M1a); empty from an older Runner.
	Kind string `json:"kind,omitempty"`
}

// RunnerDeliveryPage is one bounded response from Runner's pending-delivery
// journal. NextCursor is the sequence cursor for the next request.
type RunnerDeliveryPage struct {
	ProtocolVersion int                       `json:"protocolVersion"`
	Deliveries      []RunnerDelivery          `json:"deliveries"`
	NextCursor      uint64                    `json:"nextCursor"`
	Rejected        []RunnerDeliveryRejection `json:"rejected,omitempty"`
}

type RunnerDeliveryAcceptance struct {
	ProtocolVersion       int    `json:"protocolVersion"`
	PublicationGeneration string `json:"publicationGeneration"`
	ManifestDigest        string `json:"manifestDigest"`
	ConsumerReceiptID     string `json:"consumerReceiptId"`
}

// DeliveryProtocol is an optional client capability. It adds durable
// publication discovery and idempotent acceptance without changing the
// ordinary DownloadClient contract.
type DeliveryProtocol interface {
	ListDeliveries(ctx context.Context, after uint64, limit int) (RunnerDeliveryPage, error)
	AcceptDelivery(ctx context.Context, deliveryID string, acceptance RunnerDeliveryAcceptance) error
}

// DeliveryCapabilityProtocol advertises publication support independently of
// the delivery collection, whose retained rows may be malformed or oversized.
type DeliveryCapabilityProtocol interface {
	SupportsDeliveryV1(ctx context.Context) (bool, error)
}

// RequestTokenIdempotency is a separately negotiated Runner capability.
// Delivery publication support alone does not promise that replaying Add with
// a stable transfer token is idempotent.
type RequestTokenIdempotency interface {
	SupportsRequestTokenIdempotency(ctx context.Context) (bool, error)
}

// RequestAdmission is Runner's durable outcome for an exact transfer token,
// independent of queue and history retention.
type RequestAdmission struct {
	ProtocolVersion     int    `json:"protocolVersion"`
	RequestToken        string `json:"requestToken"`
	JobID               string `json:"jobId"`
	Generation          string `json:"generation"`
	RunnerInstanceID    string `json:"runnerInstanceId"`
	Outcome             string `json:"outcome"`
	TerminalOutcome     string `json:"terminalOutcome,omitempty"`
	PublicationPath     string `json:"publicationPath,omitempty"`
	SourceArtifactID    string `json:"sourceArtifactId,omitempty"`
	SourceGeneration    string `json:"sourceGeneration,omitempty"`
	FailureStage        string `json:"failureStage,omitempty"`
	FailureCause        string `json:"failureCause,omitempty"`
	CustodyState        string `json:"custodyState,omitempty"`
	KeepSourceOnFailure bool   `json:"keepSourceOnFailure,omitempty"`
	SourceCleanupStatus string `json:"sourceCleanupStatus,omitempty"`
	// Typed terminal fields (contract section 2.5; Runner M1a writes them as
	// Failure:* params). Empty from an older Runner, whose TerminalOutcome is
	// then read by the legacy string rule.
	FailureReason string `json:"failureReason,omitempty"`
	FetchCause    string `json:"fetchCause,omitempty"`
	BlamesRelease *bool  `json:"blamesRelease,omitempty"`
	Salvage       string `json:"salvage,omitempty"`
}

// RequestAdmissionProtocol looks up the receipt for a stable transfer token.
// A missing receipt proves non-admission only when token idempotency was
// separately negotiated for this download.
type RequestAdmissionProtocol interface {
	GetRequestAdmission(ctx context.Context, transferToken string) (RequestAdmission, bool, error)
}

// DeliverySourceCleanupRetryProtocol asks Runner to retry deletion of its
// retained processing source. Publication cleanup remains Curator-owned.
type DeliverySourceCleanupRetryProtocol interface {
	RetryDeliverySourceCleanup(ctx context.Context, deliveryID string) (RunnerDelivery, error)
}

// HTTPStatusCoder lets optional protocol negotiation distinguish an explicit
// unsupported endpoint (404) from authentication, transport, and server errors.
type HTTPStatusCoder interface {
	HTTPStatusCode() int
}

// RunnerDeliveryRejection records an unsupported entry without accepting custody
// or granting mutation authority over its files.
type RunnerDeliveryRejection struct {
	Sequence   uint64 `json:"sequence"`
	DeliveryID string `json:"deliveryId"`
	Reason     string `json:"reason"`
}

// RunnerStages is an immutable bounded snapshot from one node clock. A nil
// duration is interrupted/unknown evidence, never a fabricated elapsed value.
type RunnerStages struct {
	NodeID      string            `json:"nodeId"`
	ClockDomain string            `json:"clockDomain"`
	Spans       []RunnerStageSpan `json:"spans"`
}
type RunnerStageSpan struct {
	Stage         string `json:"stage"`
	StartedAtUnix int64  `json:"started_at_unix"`
	Milliseconds  *int64 `json:"ms,omitempty"`
}

// RunnerSourceCopy is mutable progress for one independently retained source.
// These progress facts carry no local deletion authority.
type RunnerSourceCopy struct {
	ID          string `json:"id"`
	OwnerNode   string `json:"ownerNode"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	Attempts    uint32 `json:"attempts"`
	NextRetryAt *int64 `json:"nextRetryAt,omitempty"`
}

func SameRunnerStages(a, b *RunnerStages) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.NodeID != b.NodeID || a.ClockDomain != b.ClockDomain || len(a.Spans) != len(b.Spans) {
		return false
	}
	for i, left := range a.Spans {
		right := b.Spans[i]
		if left.Stage != right.Stage || left.StartedAtUnix != right.StartedAtUnix || (left.Milliseconds == nil) != (right.Milliseconds == nil) {
			return false
		}
		if left.Milliseconds != nil && *left.Milliseconds != *right.Milliseconds {
			return false
		}
	}
	return true
}

type RunnerLifecycleTiming struct {
	OmittedIntervals map[string]uint32         `json:"omittedIntervals,omitempty"`
	Version          int                       `json:"version"`
	Intervals        []RunnerLifecycleInterval `json:"intervals"`
}
type RunnerLifecycleInterval struct {
	Stage            string  `json:"stage"`
	NodeID           string  `json:"nodeId"`
	ClockDomain      string  `json:"clockDomain"`
	StartUnixMS      int64   `json:"startUnixMs"`
	EndUnixMS        *int64  `json:"endUnixMs,omitempty"`
	StartMonotonicMS *int64  `json:"startMonotonicMs,omitempty"`
	EndMonotonicMS   *int64  `json:"endMonotonicMs,omitempty"`
	DurationMS       *int64  `json:"durationMs,omitempty"`
	State            string  `json:"state"`
	Observations     *uint32 `json:"observations,omitempty"`
}

func SameRunnerLifecycleTiming(a, b *RunnerLifecycleTiming) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Version != b.Version || len(a.Intervals) != len(b.Intervals) {
		return false
	}
	// Struct encoding is canonical for the fixed wire fields and compares nil
	// measurements separately from recorded zeros without ordering map keys.
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
