package ports

import (
	"context"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
)

// IndexerConfig is a stored Newznab/Torznab endpoint.
type IndexerConfig struct {
	ID         int64
	Name       string
	URL        string
	APIKey     string
	Protocol   string // torrent | usenet
	Categories []int
	Enabled    bool
}

// Release is one indexer result (transient; persisted only in history and
// on grab — blueprint §4.2).
type Release struct {
	Title       string    `json:"title"`
	DownloadURL string    `json:"downloadUrl"`
	InfoURL     string    `json:"infoUrl,omitempty"`
	Size        int64     `json:"size"`
	PublishDate time.Time `json:"publishDate"`
	Seeders     int       `json:"seeders"`
	Peers       int       `json:"peers"`
	Indexer     string    `json:"indexer"`
	IndexerID   int64     `json:"indexerId"`
	Protocol    string    `json:"protocol"`
}

// Indexer speaks Newznab/Torznab. RSS fetching lands in Phase 3.
type Indexer interface {
	Search(ctx context.Context, q domain.SearchQuery) ([]Release, error)
	Test(ctx context.Context) error
}
