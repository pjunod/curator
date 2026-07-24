package library

import (
	"context"
	"fmt"

	"github.com/monarr-media/monarr/internal/domain"
)

// Job kinds this service handles. Kinds are strings rather than an enum
// because a rolling restart means two versions briefly share one queue, and
// an unrecognised kind has to be a runtime decision (the queue fails it as
// permanent) rather than a compile-time one.
const (
	// JobScan is a full library reconcile. Deduped, so a slow scan never
	// stacks up behind itself and two instances never scan concurrently.
	JobScan = "library.scan"
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
	return register(JobScan, H(func(ctx context.Context, _ domain.Job) error {
		_, err := s.Scan(ctx)
		return err
	}))
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
