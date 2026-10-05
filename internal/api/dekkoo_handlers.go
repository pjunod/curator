package api

import (
	apigen "github.com/pjunod/monarr/internal/api/gen"
	"net/http"
)

// GetDekkooFeed serves publisher articles separately from identified media.
func (s *Server) GetDekkooFeed(w http.ResponseWriter, r *http.Request) {
	if s.deps.Dekkoo == nil {
		writeError(w, http.StatusServiceUnavailable, "Dekkoo feed is not available on this install")
		return
	}
	feed, err := s.deps.Dekkoo.Feed(r.Context())
	if err != nil {
		s.deps.Log.Warn("Dekkoo feed unavailable", "err", err)
		writeError(w, http.StatusBadGateway, "Could not refresh the Dekkoo feed. Try again shortly.")
		return
	}
	items := make([]apigen.EditorialArticle, 0, len(feed.Items))
	for _, item := range feed.Items {
		items = append(items, apigen.EditorialArticle{Title: item.Title, Url: item.URL,
			Summary: item.Summary, Categories: item.Categories, PublishedAt: item.PublishedAt})
	}
	writeJSON(w, http.StatusOK, apigen.EditorialFeed{Url: feed.URL, Categories: feed.Categories,
		Items: items, FetchedAt: feed.FetchedAt, Stale: feed.Stale})
}
