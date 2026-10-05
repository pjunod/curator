package ports

import (
	"context"
	"time"
)

// EditorialArticle is a publisher's article, not an identified library item.
type EditorialArticle struct {
	Title, URL, Summary string
	Categories          []string
	PublishedAt         *time.Time
}

// EditorialFeed reports when the contents were last successfully fetched.
type EditorialFeed struct {
	URL        string
	Categories []string
	Items      []EditorialArticle
	FetchedAt  time.Time
	Stale      bool
}

// EditorialSource supplies read-only discovery articles without library writes.
type EditorialSource interface {
	Feed(context.Context) (EditorialFeed, error)
}
