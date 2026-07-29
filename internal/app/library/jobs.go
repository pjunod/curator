package library

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pjunod/monarr/internal/domain"
)

// Job kinds this service handles. Kinds are strings rather than an enum
// because a rolling restart means two versions briefly share one queue, and
// an unrecognised kind has to be a runtime decision (the queue fails it as
// permanent) rather than a compile-time one.
const (
	// JobScan is a full library reconcile. Deduped, so a slow scan never
	// stacks up behind itself and two instances never scan concurrently.
	JobScan = "library.scan"
	// JobAdopt proposes matches for the unmatched folders the last scan
	// found, and applies the ones that clear the bar (ADR 0010 §4).
	//
	// This is queued rather than done inside the scan request because 600
	// directories is 600 provider searches: that exceeds any sane HTTP
	// timeout, hits provider rate limits, and a failure halfway through
	// leaves nothing. It is also the concrete workload that makes the
	// queue worth having on a single instance.
	JobAdopt = "library.adopt"
)

// JobEnqueuer is the slice of the queue this service needs. Narrow on
// purpose: the library package should not be able to start workers.
type JobEnqueuer interface {
	EnqueueUnique(ctx context.Context, j domain.Job) (bool, error)
}

// RegisterJobHandlers wires this service's job kinds into a queue. It is
// generic over the handler type so this package never imports the queue
// package — the dependency runs the other way, and the library service
// stays unable to start workers.
func RegisterJobHandlers[H ~func(ctx context.Context, j domain.Job) error](
	s *Service, register func(kind string, h H) error,
) error {
	if err := register(JobScan, H(func(ctx context.Context, _ domain.Job) error {
		return s.ScanThenAdopt(ctx)
	})); err != nil {
		return err
	}
	if err := register(JobAdopt, H(func(ctx context.Context, _ domain.Job) error {
		_, err := s.RunAdoption(ctx)
		return err
	})); err != nil {
		return err
	}
	return register(JobProbe, H(func(ctx context.Context, j domain.Job) error {
		var p probePayload
		if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
			return fmt.Errorf("probe job payload: %w", err)
		}
		if p.FileID == 0 {
			return fmt.Errorf("probe job: no file id")
		}
		return s.ProbeFile(ctx, p.FileID)
	}))
}

// ScanThenAdopt reconciles the library and then queues the matching pass.
//
// A named method rather than a closure inside RegisterJobHandlers because
// the chain is behaviour worth testing directly: an earlier version of this
// lived inline, was lost in an unrelated edit, and nothing noticed because
// no test asserted that a scan leads to a match. A scan that leaves a list
// of names is the thing ADR 0010 exists to fix, so the link between the two
// is a promise, not an implementation detail.
//
// Adoption is enqueued rather than inlined so its provider work retries and
// reports on its own terms — the scan should not be marked failed because
// TMDB rate-limited the matching.
func (s *Service) ScanThenAdopt(ctx context.Context) error {
	if _, err := s.Scan(ctx); err != nil {
		return err
	}
	return s.EnqueueAdoption(ctx)
}

// WithQueue gives the service a queue to enqueue onto. Optional: without
// one, callers that would enqueue run their work inline instead, which is
// what keeps the queue an addition rather than a hard dependency.
func (s *Service) WithQueue(q JobEnqueuer) *Service {
	s.queue = q
	return s
}

// EnqueueScan asks for a reconcile. The dedupe key means requesting a scan
// while one is already queued or running is a no-op rather than a second
// scan — which is the behaviour a "Scan now" button wants, and the behaviour
// two instances need.
//
// Without a queue configured the scan runs inline, so nothing regresses on a
// deployment that has not adopted the queue.
func (s *Service) EnqueueScan(ctx context.Context) error {
	if s.queue == nil {
		_, err := s.Scan(ctx)
		return err
	}
	queued, err := s.queue.EnqueueUnique(ctx, domain.Job{
		Kind: JobScan, DedupeKey: JobScan, Priority: 50,
	})
	if err != nil {
		return fmt.Errorf("enqueue scan: %w", err)
	}
	if !queued {
		s.log.Info("scan: already queued, request coalesced")
	}
	return nil
}

// EnqueueAdoption asks for the matching pass over the last scan's unmatched
// folders. Same coalescing as the scan, for the same reason.
func (s *Service) EnqueueAdoption(ctx context.Context) error {
	if s.queue == nil {
		_, err := s.RunAdoption(ctx)
		return err
	}
	queued, err := s.queue.EnqueueUnique(ctx, domain.Job{
		Kind: JobAdopt, DedupeKey: JobAdopt, Priority: 60,
	})
	if err != nil {
		return fmt.Errorf("enqueue adoption: %w", err)
	}
	if !queued {
		s.log.Info("adopt: already queued, request coalesced")
	}
	return nil
}
