package domain

import "time"

// TitleAlias is one provider- or operator-supplied spelling for a work.
// MarketCountry describes where a spelling is used; it is not origin.
type TitleAlias struct {
	ID            int64  `json:"id,omitempty"`
	Title         string `json:"title"`
	Source        string `json:"source"`
	SourceID      string `json:"sourceId,omitempty"`
	Language      string `json:"language,omitempty"`
	MarketCountry string `json:"marketCountry,omitempty"`
	Scope         string `json:"scope"`
	Role          string `json:"role"`
	Searchable    bool   `json:"searchable"`
}

// CountryEvidence states one country fact and why a provider believes it.
// Only origin, title_qualifier, and manual evidence may resolve a regional
// identity; network and alias-market observations remain diagnostic.
type CountryEvidence struct {
	Code   string `json:"code"`
	Source string `json:"source"`
	Basis  string `json:"basis"`
}

// IdentitySourceStatus is the durable completeness record for one provider
// snapshot. A later failed attempt deliberately makes ConventionEligible
// false without deleting the last successful aliases.
type IdentitySourceStatus struct {
	Source      string            `json:"source"`
	Countries   []CountryEvidence `json:"countries,omitempty"`
	FetchedAt   time.Time         `json:"fetchedAt,omitempty"`
	AttemptedAt time.Time         `json:"attemptedAt,omitempty"`
	RetryAfter  time.Time         `json:"retryAfter,omitempty"`
	LastError   string            `json:"lastError,omitempty"`
}

// CompleteAt reports whether a provider snapshot is complete and fresh at
// now. The seven-day age is an identity policy constant, not a setting.
func (s IdentitySourceStatus) CompleteAt(now time.Time) bool {
	return !s.FetchedAt.IsZero() && s.LastError == "" &&
		now.Sub(s.FetchedAt) <= 7*24*time.Hour
}

// MediaIdentity is the immutable matching/search view of one stored work.
type MediaIdentity struct {
	IDs       ExternalIDs            `json:"ids"`
	Title     string                 `json:"title"`
	Year      int                    `json:"year,omitempty"`
	Aliases   []TitleAlias           `json:"aliases,omitempty"`
	Countries []CountryEvidence      `json:"countries,omitempty"`
	Sources   []IdentitySourceStatus `json:"sources,omitempty"`
}

// ExternalRef is a validated provider identity used by exact metadata and
// indexer queries. Value is canonical (decimal or tt-prefixed IMDb).
type ExternalRef struct {
	Provider string `json:"provider"`
	Value    string `json:"value"`
}

// IdentityIssue preserves malformed or contradictory release attributes.
// It is evidence for diagnostics and never a positive match.
type IdentityIssue struct {
	Code     string   `json:"code"`
	Provider string   `json:"provider"`
	Values   []string `json:"values,omitempty"`
}

// MatchEvidence is the versioned snapshot persisted for a grab. It records
// what the server verified at selection time; clients cannot supply it.
type MatchEvidence struct {
	Version       int          `json:"version"`
	Matched       bool         `json:"matched"`
	Method        string       `json:"method,omitempty"`
	Code          string       `json:"code,omitempty"`
	Reason        string       `json:"reason"`
	OriginalTitle string       `json:"originalTitle,omitempty"`
	ParsedTitle   string       `json:"parsedTitle,omitempty"`
	TargetTitle   string       `json:"targetTitle,omitempty"`
	MatchedTitle  string       `json:"matchedTitle,omitempty"`
	MatchedID     *ExternalRef `json:"matchedId,omitempty"`
	SuppliedIDs   ExternalIDs  `json:"suppliedIds,omitempty"`
	TargetIDs     ExternalIDs  `json:"targetIds,omitempty"`
	Country       string       `json:"country,omitempty"`
	Warnings      []string     `json:"warnings,omitempty"`
	ConventionIDs []int64      `json:"conventionItemIds,omitempty"`
	SnapshotTimes []time.Time  `json:"snapshotTimes,omitempty"`
}
