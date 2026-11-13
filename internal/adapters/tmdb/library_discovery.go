package tmdb

import (
	"context"
	"fmt"
	"net/url"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// Library shelves cache provider metadata independently of fresh local facts.
type libraryMetadataCacheKey struct{}

var _ ports.LibraryDiscoverySource = (*Client)(nil)

func (c *Client) Related(ctx context.Context, kind domain.MediaKind, id int64) ([]ports.SearchResult, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid library seed")
	}
	path := "movie"
	if kind == domain.KindSeries {
		path = "tv"
	} else if kind != domain.KindMovie {
		return nil, fmt.Errorf("unsupported library seed kind")
	}
	ctx, _, err := c.Snapshot(ctx)
	ctx = context.WithValue(ctx, libraryMetadataCacheKey{}, true)
	if err != nil {
		return nil, err
	}
	var response struct {
		Results []struct {
			ID           int64  `json:"id"`
			Title        string `json:"title"`
			Name         string `json:"name"`
			ReleaseDate  string `json:"release_date"`
			FirstAirDate string `json:"first_air_date"`
			Overview     string `json:"overview"`
			PosterPath   string `json:"poster_path"`
		} `json:"results"`
	}
	if err = c.read(ctx, fmt.Sprintf("/%s/%d/recommendations", path, id), url.Values{"page": {"1"}}, &response, true); err != nil {
		return nil, err
	}
	out := []ports.SearchResult{}
	seen := map[int64]bool{id: true}
	for _, r := range response.Results {
		if r.ID <= 0 || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		title, date := r.Title, r.ReleaseDate
		if kind == domain.KindSeries {
			title, date = r.Name, r.FirstAirDate
		}
		if title == "" {
			continue
		}
		out = append(out, ports.SearchResult{Kind: kind, TMDBID: r.ID, Title: title, Year: yearOf(date), Overview: r.Overview, PosterPath: r.PosterPath, Source: "tmdb"})
		if len(out) == 30 {
			break
		}
	}
	return out, nil
}

func (c *Client) MovieCollection(ctx context.Context, id int64) (ports.MovieCollection, error) {
	if id <= 0 {
		return ports.MovieCollection{}, fmt.Errorf("invalid movie seed")
	}
	ctx, _, err := c.Snapshot(ctx)
	ctx = context.WithValue(ctx, libraryMetadataCacheKey{}, true)
	if err != nil {
		return ports.MovieCollection{}, err
	}
	var movie struct {
		ID         int64 `json:"id"`
		Collection *struct {
			ID int64 `json:"id"`
		} `json:"belongs_to_collection"`
	}
	if err = c.read(ctx, fmt.Sprintf("/movie/%d", id), nil, &movie, true); err != nil {
		return ports.MovieCollection{}, err
	}
	if movie.ID != id {
		return ports.MovieCollection{}, fmt.Errorf("provider returned a different movie identity")
	}
	if movie.Collection == nil || movie.Collection.ID <= 0 {
		return ports.MovieCollection{Items: []ports.SearchResult{}}, nil
	}
	var collection struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Parts []struct {
			ID          int64  `json:"id"`
			Title       string `json:"title"`
			ReleaseDate string `json:"release_date"`
			Overview    string `json:"overview"`
			PosterPath  string `json:"poster_path"`
		} `json:"parts"`
	}
	if err = c.read(ctx, fmt.Sprintf("/collection/%d", movie.Collection.ID), nil, &collection, true); err != nil {
		return ports.MovieCollection{}, err
	}
	if collection.ID != movie.Collection.ID {
		return ports.MovieCollection{}, fmt.Errorf("provider returned a different collection identity")
	}
	if len(collection.Parts) > 100 {
		return ports.MovieCollection{}, fmt.Errorf("collection exceeds the 100-title browsing bound")
	}
	out := ports.MovieCollection{ID: collection.ID, Name: collection.Name, Items: []ports.SearchResult{}}
	seen := map[int64]bool{}
	hasSeed := false
	for _, r := range collection.Parts {
		if r.ID == id {
			hasSeed = true
		}
		if r.ID <= 0 || seen[r.ID] || r.Title == "" {
			continue
		}
		seen[r.ID] = true
		out.Items = append(out.Items, ports.SearchResult{Kind: domain.KindMovie, TMDBID: r.ID, Title: r.Title, Year: yearOf(r.ReleaseDate), Overview: r.Overview, PosterPath: r.PosterPath, Source: "tmdb"})
	}
	if !hasSeed {
		return ports.MovieCollection{}, fmt.Errorf("collection does not contain the selected seed")
	}
	return out, nil
}
