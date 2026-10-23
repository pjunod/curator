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
	ProtocolVersion       int                 `json:"protocolVersion,omitempty"`
	Sequence              uint64              `json:"sequence"`
	DeliveryID            string              `json:"deliveryId"`
	PublicationGeneration string              `json:"publicationGeneration"`
	SourceArtifactID      string              `json:"sourceArtifactId,omitempty"`
	SourceGeneration      string              `json:"sourceGeneration,omitempty"`
	ArtifactID            string              `json:"artifactId"`
	JobID                 string              `json:"jobId"`
	RequestToken          string              `json:"requestToken"`
	RunnerInstanceID      string              `json:"runnerInstanceId"`
	PublicationPath       string              `json:"publicationPath"`
	ManifestDigest        string              `json:"manifestDigest"`
	Files                 []RunnerFile        `json:"files"`
	TerminalOutcome       string              `json:"terminalOutcome,omitempty"`
	FailureStage          string              `json:"failureStage,omitempty"`
	FailureCause          string              `json:"failureCause,omitempty"`
	KeepSourceOnFailure   bool                `json:"keepSourceOnFailure,omitempty"`
	FileOutcomes          []RunnerFileOutcome `json:"fileOutcomes,omitempty"`
	ReadyAt               time.Time           `json:"readyAt"`
	SourceCleanup         json.RawMessage     `json:"sourceCleanup,omitempty"`
}

// RunnerDeliveryPage is one bounded response from Runner's pending-delivery
// journal. NextCursor is the sequence cursor for the next request.
type RunnerDeliveryPage struct {
	ProtocolVersion int              `json:"protocolVersion"`
	Deliveries      []RunnerDelivery `json:"deliveries"`
	NextCursor      uint64           `json:"nextCursor"`
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
}

// RequestAdmissionProtocol looks up the receipt for a stable transfer token.
// A missing receipt proves non-admission only when token idempotency was
// separately negotiated for this download.
type RequestAdmissionProtocol interface {
	GetRequestAdmission(ctx context.Context, transferToken string) (RequestAdmission, bool, error)
}

// DeliveryLookupProtocol exposes the immutable delivery and mutable cleanup
// progress after it leaves the pending collection endpoint.
type DeliveryLookupProtocol interface {
	GetDelivery(ctx context.Context, deliveryID string) (RunnerDelivery, error)
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
