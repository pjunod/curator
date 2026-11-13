// Package availability owns the shared, read-only browsing projection.
package availability

import (
	"context"
	"github.com/pjunod/monarr/internal/domain"
)

type Reader interface {
	LookupAvailability(context.Context, []domain.AvailabilityTarget) ([]domain.Availability, error)
}

type Service struct{ reader Reader }

func New(reader Reader) *Service { return &Service{reader: reader} }

// Lookup preserves candidate order. Unreadable facts are Unknown, never absence.
func (s *Service) Lookup(ctx context.Context, targets []domain.AvailabilityTarget) []domain.Availability {
	unknown := make([]domain.Availability, len(targets))
	for i := range unknown {
		unknown[i] = domain.Availability{Catalog: "unknown", Presence: "unknown", Reason: "library_unavailable", Acquisition: "unknown"}
	}
	if s == nil || s.reader == nil || len(targets) > 100 {
		return unknown
	}
	values, err := s.reader.LookupAvailability(ctx, targets)
	if err != nil || len(values) != len(targets) {
		return unknown
	}
	return values
}
