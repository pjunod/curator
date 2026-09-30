package tmdb

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/recommendation"
	"github.com/pjunod/monarr/internal/ports"
)

type recommendationSnapshotKey struct{}
type recommendationSnapshot struct {
	key   string
	calls atomic.Int64
}

func (c *Client) Snapshot(ctx context.Context) (context.Context, string, error) {
	key, err := c.keyFn(ctx)
	if err != nil {
		return ctx, "", err
	}
	if key == "" {
		return ctx, "", ports.ErrProviderNotConfigured
	}
	return context.WithValue(ctx, recommendationSnapshotKey{}, &recommendationSnapshot{key: key}), fmt.Sprintf("%x", sha256.Sum256([]byte(key))), nil
}
func (c *Client) ResolveKeyword(ctx context.Context, alias string) ([]ports.Keyword, error) {
	var result struct {
		Results []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"results"`
	}
	err := c.read(ctx, "/search/keyword", url.Values{"query": {alias}}, &result, true)
	out := make([]ports.Keyword, 0)
	for _, k := range result.Results {
		if strings.EqualFold(strings.TrimSpace(k.Name), alias) && k.ID > 0 {
			out = append(out, ports.Keyword{ID: k.ID, Name: k.Name})
		}
	}
	return out, err
}
func (c *Client) Candidates(ctx context.Context, r ports.CandidateRequest) (ports.CandidatePage, error) {
	params := url.Values{"page": {strconv.Itoa(r.Page)}, "language": {"en-US"}}
	path := fmt.Sprintf("/tv/%d/%s", r.SeedID, r.Path)
	if r.Path == "theme" {
		path = "/discover/tv"
		params.Set("with_keywords", strconv.FormatInt(r.KeywordID, 10))
		params.Set("sort_by", "vote_count.desc")
		f := r.Filters
		if f.OriginalLanguage != nil {
			params.Set("with_original_language", *f.OriginalLanguage)
		}
		if f.YearFrom != nil {
			params.Set("first_air_date.gte", fmt.Sprintf("%d-01-01", *f.YearFrom))
		}
		if f.YearTo != nil {
			params.Set("first_air_date.lte", fmt.Sprintf("%d-12-31", *f.YearTo))
		}
		if len(f.Genres) > 0 {
			ids := []string{}
			for _, id := range f.Genres {
				ids = append(ids, strconv.Itoa(id))
			}
			params.Set("with_genres", strings.Join(ids, "|"))
		}
	} else if r.Path != "recommendations" && r.Path != "similar" {
		return ports.CandidatePage{}, fmt.Errorf("unsupported candidate path")
	}
	var result struct {
		TotalPages int `json:"total_pages"`
		Results    []struct {
			ID           int64  `json:"id"`
			Name         string `json:"name"`
			FirstAirDate string `json:"first_air_date"`
			Overview     string `json:"overview"`
			PosterPath   string `json:"poster_path"`
		} `json:"results"`
	}
	err := c.read(ctx, path, params, &result, true)
	out := ports.CandidatePage{TotalPages: result.TotalPages, Items: []ports.SearchResult{}}
	for _, v := range result.Results {
		if v.ID > 0 {
			out.Items = append(out.Items, ports.SearchResult{Kind: domain.KindSeries, TMDBID: v.ID, Source: "tmdb", HydrationSource: "tmdb", Title: v.Name, Year: yearOf(v.FirstAirDate), Overview: recommendation.Excerpt(v.Overview, 8192), PosterPath: v.PosterPath})
		}
	}
	return out, err
}
func (c *Client) Facts(ctx context.Context, id int64) (recommendation.Facts, error) {
	var v struct {
		ID               int64  `json:"id"`
		Name             string `json:"name"`
		Overview         string `json:"overview"`
		PosterPath       string `json:"poster_path"`
		FirstAirDate     string `json:"first_air_date"`
		OriginalLanguage string `json:"original_language"`
		Genres           []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
		Keywords struct {
			Results []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"results"`
		} `json:"keywords"`
		ExternalIDs struct {
			TVDB int64  `json:"tvdb_id"`
			IMDB string `json:"imdb_id"`
		} `json:"external_ids"`
	}
	if err := c.read(ctx, fmt.Sprintf("/tv/%d", id), url.Values{"append_to_response": {"keywords,external_ids"}, "language": {"en-US"}}, &v, true); err != nil {
		return recommendation.Facts{}, err
	}
	if v.ID != id {
		return recommendation.Facts{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict}
	}
	f := recommendation.Facts{IDs: domain.ExternalIDs{TMDB: v.ID, TVDB: v.ExternalIDs.TVDB, IMDB: v.ExternalIDs.IMDB}, Title: v.Name, Overview: recommendation.Excerpt(v.Overview, 8192), PosterPath: v.PosterPath, Year: yearOf(v.FirstAirDate), OriginalLanguage: v.OriginalLanguage, FetchedAt: time.Now(), KeywordIDs: map[string]int64{}}
	for _, g := range v.Genres {
		f.GenreIDs = append(f.GenreIDs, g.ID)
		f.GenreNames = append(f.GenreNames, g.Name)
	}
	for _, k := range v.Keywords.Results {
		if len(f.Keywords) >= 64 {
			break
		}
		name := recommendation.Excerpt(k.Name, 128)
		f.Keywords = append(f.Keywords, name)
		f.KeywordIDs[strings.ToLower(name)] = k.ID
	}
	return f, nil
}
func (c *Client) ResolveSeed(ctx context.Context, ref domain.ExternalRef) (recommendation.Facts, error) {
	id, err := strconv.ParseInt(ref.Value, 10, 64)
	if err != nil || id <= 0 {
		return recommendation.Facts{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	}
	if ref.Provider == "tvdb" {
		var result struct {
			Results []struct {
				ID int64 `json:"id"`
			} `json:"tv_results"`
		}
		if err = c.read(ctx, "/find/"+ref.Value, url.Values{"external_source": {"tvdb_id"}}, &result, true); err != nil {
			return recommendation.Facts{}, err
		}
		if len(result.Results) == 0 {
			return recommendation.Facts{}, &ports.RemoteError{Category: ports.RemoteUnsupportedHydration}
		}
		if len(result.Results) != 1 {
			return recommendation.Facts{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict}
		}
		id = result.Results[0].ID
	} else if ref.Provider != "tmdb" {
		return recommendation.Facts{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	}
	f, err := c.Facts(ctx, id)
	if err == nil && ref.Provider == "tvdb" && f.IDs.TVDB != mustID(ref.Value) {
		return recommendation.Facts{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict}
	}
	return f, err
}
func mustID(s string) int64 { id, _ := strconv.ParseInt(s, 10, 64); return id }
