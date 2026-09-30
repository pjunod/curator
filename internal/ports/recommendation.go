package ports

import (
	"context"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/recommendation"
)

type Keyword struct {
	ID   int64
	Name string
}
type CandidateRequest struct {
	Path              string
	KeywordID, SeedID int64
	Page              int
	Filters           recommendation.Filters
}
type CandidatePage struct {
	Items      []SearchResult
	TotalPages int
}
type RecommendationSource interface {
	Snapshot(context.Context) (context.Context, string, error)
	ResolveKeyword(context.Context, string) ([]Keyword, error)
	Candidates(context.Context, CandidateRequest) (CandidatePage, error)
	Facts(context.Context, int64) (recommendation.Facts, error)
	ResolveSeed(context.Context, domain.ExternalRef) (recommendation.Facts, error)
}
type Ownership struct {
	State         string
	LibraryItemID int64
	Conflict      bool
}
type RecommendationOwnership interface {
	LookupSeries(context.Context, []domain.ExternalIDs) ([]Ownership, error)
}
type SentenceEncoder interface {
	ModelID() string
	Encode(context.Context, []string) ([][]float32, error)
	// Generation fences configuration and installed model identity. Runtime
	// failures change availability state without changing this generation.
	State() (string, uint64)
	Close() error
}
