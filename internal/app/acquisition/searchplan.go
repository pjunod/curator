package acquisition

import (
	"context"
	"fmt"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// queriesForIndexer converts the domain's ordered alternatives into wire
// queries that one indexer's advertised capabilities can actually answer.
// Unknown capabilities deliberately permit one generic canonical probe only.
func queriesForIndexer(ctx context.Context, indexer ports.Indexer, w domain.Wantable, interactive bool) ([]domain.SearchQuery, []string, error) {
	if _, ok := w.(domain.BookWantable); ok {
		return domain.PlanSearch(w), nil, nil
	}
	plan := domain.PlanVideoSearch(w)
	if len(plan.Titles) == 0 {
		return nil, nil, nil
	}
	provider, ok := indexer.(ports.IndexerCapabilitiesProvider)
	if !ok {
		q := plan.Titles[0]
		q.Mode = "generic"
		q.SeasonSet, q.EpisodeSet = false, false
		return []domain.SearchQuery{q}, nil, nil
	}
	caps, err := provider.Capabilities(ctx)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if caps.Degraded {
		warnings = append(warnings, "capability discovery failed; used generic fallback")
	}
	if !caps.Generic.Known && !caps.TV.Known && !caps.Movie.Known {
		q := plan.Titles[0]
		q.Mode = "generic"
		q.SeasonSet, q.EpisodeSet = false, false
		return []domain.SearchQuery{q}, warnings, nil
	}

	idLimit, titleLimit := 1, 2
	if interactive {
		idLimit, titleLimit = 2, 3
	}
	queries := make([]domain.SearchQuery, 0, idLimit+titleLimit)
	for _, q := range plan.IDs {
		if len(queries) >= idLimit {
			break
		}
		mode := capabilityForKind(caps, q.Kind)
		if !mode.Known || !mode.Available || q.ID == nil || !mode.Parameters[q.ID.Provider+"id"] {
			continue
		}
		q.Mode = modeName(q.Kind)
		applyCoverageCapabilities(&q, mode)
		queries = append(queries, q)
	}

	titles := 0
	for _, q := range plan.Titles {
		if titles >= titleLimit {
			break
		}
		mode := capabilityForKind(caps, q.Kind)
		switch {
		case mode.Known && mode.Available && mode.Parameters["q"]:
			q.Mode = modeName(q.Kind)
			applyCoverageCapabilities(&q, mode)
		case caps.Generic.Known && caps.Generic.Available && caps.Generic.Parameters["q"]:
			q.Mode = "generic"
			q.SeasonSet, q.EpisodeSet = false, false
		default:
			continue
		}
		queries = append(queries, q)
		titles++
	}
	if len(queries) == 0 {
		return nil, warnings, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery, Cause: fmt.Errorf("indexer advertises no compatible search mode")}
	}
	return queries, warnings, nil
}

func capabilityForKind(caps ports.IndexerCapabilities, kind domain.MediaKind) ports.IndexerSearchCapability {
	if kind == domain.KindSeries {
		return caps.TV
	}
	return caps.Movie
}

func modeName(kind domain.MediaKind) string {
	if kind == domain.KindSeries {
		return "tv"
	}
	return "movie"
}

func applyCoverageCapabilities(q *domain.SearchQuery, capability ports.IndexerSearchCapability) {
	if !capability.Parameters["season"] {
		q.SeasonSet = false
	}
	if !capability.Parameters["ep"] {
		q.EpisodeSet = false
	}
}
