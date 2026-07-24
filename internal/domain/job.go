package domain

import "time"

// JobState is where a unit of work is in its lifecycle.
type JobState string

// The four states. There is no "running" distinct from "leased": holding the
// lease *is* running, which is what makes a dead node's work recoverable —
// the lease expires and the row returns to the queue (ADR 0008 §1).
const (
	JobQueued JobState = "queued"
	JobLeased JobState = "leased"
	JobDone   JobState = "done"
	JobFailed JobState = "failed"
)

// Job is one claimable unit of work.
//
// The reason this type exists at all: nothing in Monarr claimed a unit of
// work before it. The scheduler's timers recorded what ran but never claimed
// anything, so a second instance ran every task twice. A claim is what makes
// concurrent instances safe — and durable retries, visible failures, and
// work that survives a restart are worth having on one instance regardless.
type Job struct {
	ID      int64
	Kind    string
	Payload string // JSON, interpreted by the handler for Kind
	State   JobState

	// Priority orders the queue; lower runs first. Interactive work
	// (a user pressed a button) should outrank background sweeps.
	Priority int64
	RunAfter time.Time

	Attempts    int64
	MaxAttempts int64
	LastError   string

	// DedupeKey gives "exactly one cluster-wide" for a job that must not
	// run twice concurrently — a singleton job without a singleton node,
	// so there is no leader to elect and no failover to operate.
	DedupeKey string
	// RequiredCapability restricts a job to nodes advertising a tag. For
	// Monarr's workload the tags are mounts and toolchains, not hardware:
	// with no transcode to place, the only thing capability expresses is
	// which node can see which files.
	RequiredCapability string
	// AffinityNode pins work to exactly one node.
	AffinityNode string

	LeaseOwner     string
	LeaseExpiresAt time.Time

	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
}
