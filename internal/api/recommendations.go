package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/recommendation"
	"github.com/pjunod/monarr/internal/domain"
	rules "github.com/pjunod/monarr/internal/domain/recommendation"
	"github.com/pjunod/monarr/internal/ports"
)

func decodeRecommendation(w http.ResponseWriter, r *http.Request) (rules.Request, error) {
	var body struct {
		Kind  domain.MediaKind `json:"kind"`
		Query json.RawMessage  `json:"query"`
		Seed  *struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
		} `json:"seed"`
		Filters json.RawMessage `json:"filters"`
		Limit   json.RawMessage `json:"limit"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return rules.Request{}, errors.New("invalid recommendation JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return rules.Request{}, errors.New("only one JSON object is accepted")
	}
	out := rules.Request{Kind: body.Kind, Explicit: map[string]bool{}}
	if len(body.Query) > 0 {
		if string(body.Query) == "null" || json.Unmarshal(body.Query, &out.Query) != nil {
			return out, errors.New("query must be a string")
		}
	}
	if len(body.Limit) > 0 {
		if string(body.Limit) == "null" || json.Unmarshal(body.Limit, &out.Limit) != nil || out.Limit < 1 {
			return out, errors.New("limit must be 1–20")
		}
	}
	if body.Seed != nil {
		out.Seed = &domain.ExternalRef{Provider: body.Seed.Provider, Value: body.Seed.ID}
	}
	if len(body.Filters) > 0 {
		if string(body.Filters) == "null" {
			return out, errors.New("filters must be an object")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body.Filters, &fields) != nil {
			return out, errors.New("filters must be an object")
		}
		for key, value := range fields {
			switch key {
			case "theme", "originalLanguage", "yearFrom", "yearTo":
			case "centralThemeOnly", "genres", "excludeTeenFocus", "hideInLibrary":
				if string(value) == "null" {
					return out, fmt.Errorf("%s cannot be null", key)
				}
			default:
				return out, fmt.Errorf("unknown filter %s", key)
			}
			out.Explicit[key] = true
		}
		if err := json.Unmarshal(body.Filters, &out.Filters); err != nil {
			return out, errors.New("invalid filter value")
		}
	}
	return out, nil
}

func (s *Server) RecommendSeries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := decodeRecommendation(w, r)
	if err != nil {
		writeCodedError(w, 400, "invalid_recommendation_request", err.Error())
		return
	}
	if s.deps.Recommendations == nil {
		writeCodedError(w, 503, "provider_unavailable", "Recommendations are unavailable.")
		return
	}
	value, err := s.deps.Recommendations.Search(r.Context(), request)
	if err != nil {
		code, status, message := "provider_unavailable", 503, "The provider is unavailable; retry the search."
		var remote *ports.RemoteError
		switch {
		case errors.Is(err, recommendation.ErrInvalidRequest):
			code, status, message = "invalid_recommendation_request", 400, err.Error()
		case errors.Is(err, recommendation.ErrBusy):
			code, status, message = "recommendations_busy", 429, "Another discovery search is running; retry shortly."
			w.Header().Set("Retry-After", "2")
		case errors.Is(err, recommendation.ErrStateChanged):
			code, status, message = "recommendation_state_changed", 503, "Discovery settings changed; retry the search."
		case errors.Is(err, recommendation.ErrUnavailable), errors.Is(err, ports.ErrProviderNotConfigured):
			code, status, message = "provider_unavailable", 503, "Configure TMDB or retry when the provider is available."
		case errors.As(err, &remote):
			switch remote.Category {
			case ports.RemoteNotFound:
				code, status, message = "seed_not_found", 404, "The selected seed was not found."
			case ports.RemoteIdentityConflict:
				code, status, message = "identity_conflict", 409, "The seed's verified identities conflict."
			case ports.RemoteUnsupportedQuery, ports.RemoteUnsupportedHydration:
				code, status, message = "unsupported_seed", 422, "The selected seed cannot map to a verified TMDB series."
			default:
				code, status, message = "provider_unavailable", 503, "The provider is unavailable."
			}
		default:
			if r.Context().Err() != nil {
				return
			}
		}
		writeCodedError(w, status, code, message)
		return
	}
	f := value.Applied.Filters
	genres := f.Genres
	if genres == nil {
		genres = []int{}
	}
	applied := apigen.RecommendationApplied{Query: value.Applied.Query, RankingText: value.Applied.RankingText, Limit: value.Applied.Limit, InterpretationVersion: value.Applied.InterpretationVersion, Filters: apigen.RecommendationFilters{CentralThemeOnly: &f.CentralThemeOnly, ExcludeTeenFocus: &f.ExcludeTeenFocus, HideInLibrary: &f.HideInLibrary, Genres: &genres, OriginalLanguage: f.OriginalLanguage, YearFrom: f.YearFrom, YearTo: f.YearTo}}
	if f.Theme != nil {
		theme := apigen.RecommendationFiltersTheme(*f.Theme)
		applied.Filters.Theme = &theme
	}
	if seed := value.Applied.Seed; seed != nil {
		applied.Seed = &apigen.RecommendationSeed{Provider: apigen.RecommendationSeedProvider(seed.Provider), Id: seed.Value}
	}
	out := apigen.RecommendationResponse{State: apigen.RecommendationResponseState(value.State), Applied: applied, Ranking: apigen.RecommendationResponseRanking(value.Ranking), ModelState: value.ModelState, Coverage: apigen.RecommendationResponseCoverage(value.Coverage), RetrievedCount: value.RetrievedCount, CheckedCount: value.CheckedCount, EligibleCount: value.EligibleCount, HiddenOwnedCount: value.HiddenOwnedCount, ReturnedCount: len(value.Results), Warnings: []apigen.RecommendationWarning{}, Results: []apigen.RecommendationResult{}}
	for _, warning := range value.Warnings {
		out.Warnings = append(out.Warnings, apigen.RecommendationWarning{Code: warning.Code, Message: warning.Message})
	}
	for _, result := range value.Results {
		item := result.Item
		source, hydration := item.Source, item.HydrationSource
		sr := apigen.SearchResult{Kind: apigen.MediaKind("series"), TmdbId: item.TMDBID, Title: item.Title, Year: item.Year, Overview: item.Overview, PosterPath: item.PosterPath, InLibrary: result.Ownership.State == "present", Source: &source, HydrationSource: &hydration}
		if item.TVDBID > 0 {
			sr.TvdbId = &item.TVDBID
		}
		sr.ImdbId = optStr(item.IMDBID)
		row := apigen.RecommendationResult{Key: fmt.Sprintf("series:tmdb:%d", item.TMDBID), Item: sr, Ownership: apigen.RecommendationResultOwnership(result.Ownership.State), Addability: "supported", ThemeEvidence: apigen.RecommendationResultThemeEvidence(result.Evidence.State), FetchedAt: result.Facts.FetchedAt, Reasons: []apigen.RecommendationReason{}}
		if result.Ownership.LibraryItemID > 0 {
			row.LibraryItemId = &result.Ownership.LibraryItemID
		}
		if result.Ownership.Conflict {
			row.Addability = "conflict"
		}
		if result.Evidence.Field != "" {
			row.Reasons = append(row.Reasons, apigen.RecommendationReason{Code: "theme_evidence", Source: "tmdb", Field: result.Evidence.Field, Value: result.Evidence.Value, FetchedAt: result.Facts.FetchedAt})
		}
		if value.Ranking == "semantic" && value.SeedCompared {
			row.Reasons = append(row.Reasons, apigen.RecommendationReason{Code: "similar_description", Source: "local", Field: "description", Value: "Similar description to the selected series", FetchedAt: result.Facts.FetchedAt})
		}
		out.Results = append(out.Results, row)
	}
	writeJSON(w, 200, out)
}

func (s *Server) RecommendationStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out := apigen.RecommendationStatus{ModelState: "disabled", Themes: []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	}{}}
	if s.deps.Recommendations != nil {
		state, configured := s.deps.Recommendations.Status(r.Context())
		out.ModelState = apigen.RecommendationStatusModelState(state)
		out.ProviderConfigured = configured
	}
	if s.deps.RecommendationModel != nil {
		_, out.Message = s.deps.RecommendationModel.Detail()
	}
	for _, t := range rules.Themes {
		out.Themes = append(out.Themes, struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		}{t.Key, t.Label})
	}
	writeJSON(w, 200, out)
}
func (s *Server) InstallRecommendationModel(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecommendationModel == nil {
		writeCodedError(w, 503, "model_unavailable", "The standalone encoder is unavailable.")
		return
	}
	if err := s.deps.RecommendationModel.Install(); err != nil {
		writeCodedError(w, 503, "model_unavailable", strings.TrimSpace(err.Error()))
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
func (s *Server) RemoveRecommendationModel(w http.ResponseWriter, r *http.Request) {
	if s.deps.RecommendationModel != nil {
		if err := s.deps.RecommendationModel.Uninstall(); err != nil {
			writeCodedError(w, 409, "model_enabled", err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
