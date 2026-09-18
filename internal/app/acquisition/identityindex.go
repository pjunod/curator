package acquisition

import (
	"context"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/matcher"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/ports"
)

func (s *Service) allIdentity(ctx context.Context, now time.Time) (matcher.IdentityIndex, error) {
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		revision, err := s.db.IdentityRevision(ctx)
		if err != nil {
			return matcher.IdentityIndex{}, err
		}
		if s.identityCache.Works != nil && s.identityRevision == revision {
			index := s.identityCache
			index.Now = now
			return index, nil
		}
		items, err := s.db.ListMediaItems(ctx, "")
		if err != nil {
			return matcher.IdentityIndex{}, err
		}
		confirmedRevision, err := s.db.IdentityRevision(ctx)
		if err != nil {
			return matcher.IdentityIndex{}, err
		}
		if confirmedRevision != revision {
			continue
		}
		index := matcher.NewIdentityIndex(items, now)
		s.identityCache, s.identityRevision = index, revision
		return index, nil
	}
	return matcher.IdentityIndex{}, fmt.Errorf("library identity changed while building release match index")
}

func releaseMatch(r ports.Release, parsed parser.Parsed, want domain.Wantable, index matcher.IdentityIndex) matcher.MatchDecision {
	if _, ok := want.(domain.BookWantable); ok {
		if len(matcher.Match(parsed, []domain.Wantable{want})) > 0 {
			return matcher.MatchDecision{Matched: true, Method: "title", Reason: "Matched book title and author"}
		}
		return matcher.MatchDecision{Code: "title_mismatch", Reason: "Release does not match the requested book"}
	}
	return matcher.Evaluate(matcher.ReleaseEvidence{Parsed: parsed, IDs: r.IDs, IDIssues: r.IDIssues}, want, index)
}

func matchEvidence(parsed parser.Parsed, want domain.Wantable, release ports.Release, decision matcher.MatchDecision) domain.MatchEvidence {
	identity := identityForWantable(want)
	return domain.MatchEvidence{
		Version: 1, Matched: decision.Matched, Method: decision.Method, Code: decision.Code,
		Reason: decision.Reason, OriginalTitle: parsed.RawTitle, ParsedTitle: parsed.Title,
		TargetTitle: identity.Title, MatchedTitle: decision.MatchedTitle, MatchedID: decision.MatchedID,
		SuppliedIDs: release.IDs, TargetIDs: identity.IDs, Country: decision.Country,
		Warnings: decision.Warnings, ConventionIDs: decision.ConventionIDs, SnapshotTimes: decision.SnapshotTimes,
	}
}

func identityForWantable(w domain.Wantable) domain.MediaIdentity {
	switch value := w.(type) {
	case domain.MovieWantable:
		return value.Identity
	case domain.EpisodeWantable:
		return value.Identity
	case domain.SeasonWantable:
		return value.Identity
	case domain.BookWantable:
		return domain.MediaIdentity{Title: value.Title, Year: value.Year}
	default:
		return domain.MediaIdentity{}
	}
}

func wantableWithIdentity(w domain.Wantable, identity domain.MediaIdentity) domain.Wantable {
	switch value := w.(type) {
	case domain.MovieWantable:
		value.Identity, value.Title, value.Year = identity, identity.Title, identity.Year
		return value
	case domain.EpisodeWantable:
		value.Identity, value.Title, value.Year = identity, identity.Title, identity.Year
		return value
	case domain.SeasonWantable:
		value.Identity, value.Title, value.Year = identity, identity.Title, identity.Year
		return value
	default:
		return w
	}
}
