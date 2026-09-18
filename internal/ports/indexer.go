package ports

import (
	"context"
	"time"

	"github.com/pjunod/monarr/internal/domain"
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
	GUID        string                 `json:"guid,omitempty"`
	Title       string                 `json:"title"`
	DownloadURL string                 `json:"downloadUrl"`
	InfoURL     string                 `json:"infoUrl,omitempty"`
	Size        int64                  `json:"size"`
	PublishDate time.Time              `json:"publishDate"`
	Seeders     int                    `json:"seeders"`
	Peers       int                    `json:"peers"`
	Indexer     string                 `json:"indexer"`
	IndexerID   int64                  `json:"indexerId"`
	Protocol    string                 `json:"protocol"`
	IDs         domain.ExternalIDs     `json:"ids,omitempty"`
	IDIssues    []domain.IdentityIssue `json:"idIssues,omitempty"`
}

// IndexerSearchCapability describes one advertised search mode. Known=false
// means caps were absent or malformed; Available=false with Known=true is an
// explicit prohibition.
type IndexerSearchCapability struct {
	Known      bool
	Available  bool
	Parameters map[string]bool
}

type IndexerCapabilities struct {
	Generic   IndexerSearchCapability
	TV        IndexerSearchCapability
	Movie     IndexerSearchCapability
	FetchedAt time.Time
	Degraded  bool
}

// IndexerCapabilitiesProvider is optional so non-Newznab adapters and tests
// keep their existing contract. A valid snapshot controls which wire modes
// and parameters the caller may use.
type IndexerCapabilitiesProvider interface {
	Capabilities(context.Context) (IndexerCapabilities, error)
}

// IndexerCapabilityInvalidator lets a protocol adapter downgrade a mode that
// the server advertised but rejected at runtime.
type IndexerCapabilityInvalidator interface {
	InvalidateSearchCapability(mode string)
}

// Indexer speaks Newznab/Torznab.
type Indexer interface {
	Search(ctx context.Context, q domain.SearchQuery) ([]Release, error)
	// FetchRSS returns the indexer's recent releases (empty-query search —
	// the standard Torznab RSS mode), for the Phase 3 sync loop.
	FetchRSS(ctx context.Context) ([]Release, error)
	Test(ctx context.Context) error
}
