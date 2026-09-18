package acquisition

import (
	"context"
	"errors"
	"fmt"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// SearchStatus distinguishes a complete empty result from a result set built
// while one or more indexers or query modes failed.
type SearchStatus struct {
	Partial bool
	Reasons []string
}

func (s *Service) executeIndexerSearch(ctx context.Context, indexer ports.Indexer, w domain.Wantable, interactive bool, stop func([]ports.Release) bool) ([]ports.Release, []string) {
	cctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
	defer cancel()
	queries, reasons, err := queriesForIndexer(cctx, indexer, w, interactive)
	if err != nil {
		return nil, []string{searchFailureReason(err)}
	}
	var releases []ports.Release
	for tier, q := range queries {
		rs, searchErr := indexer.Search(cctx, q)
		if searchErr != nil {
			reasons = append(reasons, fmt.Sprintf("tier %d: %s", tier+1, searchFailureReason(searchErr)))
			if terminalSearchError(searchErr) {
				break
			}
			if unsupportedSearchError(searchErr) && q.Mode != "generic" {
				if invalidator, ok := indexer.(ports.IndexerCapabilityInvalidator); ok {
					invalidator.InvalidateSearchCapability(q.Mode)
				}
				fallback, ok := genericCanonicalQuery(w)
				if ok {
					fallbackRows, fallbackErr := indexer.Search(cctx, fallback)
					if fallbackErr != nil {
						reasons = append(reasons, "generic fallback: "+searchFailureReason(fallbackErr))
					} else {
						releases = append(releases, fallbackRows...)
						if stop != nil && stop(fallbackRows) {
							break
						}
					}
				}
				break
			}
			continue
		}
		releases = append(releases, rs...)
		if stop != nil && stop(rs) {
			break
		}
	}
	return releases, reasons
}

func genericCanonicalQuery(w domain.Wantable) (domain.SearchQuery, bool) {
	plan := domain.PlanVideoSearch(w)
	if len(plan.Titles) == 0 {
		return domain.SearchQuery{}, false
	}
	q := plan.Titles[0]
	q.Mode = "generic"
	q.SeasonSet, q.EpisodeSet = false, false
	return q, true
}

func terminalSearchError(err error) bool {
	var remote *ports.RemoteError
	return errors.As(err, &remote) && (remote.Category == ports.RemoteAuth || remote.Category == ports.RemoteRateLimit)
}

func unsupportedSearchError(err error) bool {
	var remote *ports.RemoteError
	return errors.As(err, &remote) && remote.Category == ports.RemoteUnsupportedQuery
}

func searchFailureReason(err error) string {
	var remote *ports.RemoteError
	if errors.As(err, &remote) {
		return remote.Category
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "indexer error"
}
