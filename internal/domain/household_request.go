package domain

import "time"

// RequestScope freezes regular aired coordinates at submission. A later episode
// becoming aired never expands an already approved household request.
type RequestEpisode struct {
	Season  int `json:"season"`
	Episode int `json:"episode"`
}
type RequestScope struct {
	Episodes []RequestEpisode `json:"episodes,omitempty"`
}
type HouseholdRequest struct {
	ID              int64        `json:"id"`
	Revision        int64        `json:"revision"`
	Kind            MediaKind    `json:"kind"`
	TMDBID          int64        `json:"tmdbId"`
	IDs             ExternalIDs  `json:"-"`
	Scope           RequestScope `json:"scope"`
	Title           string       `json:"title"`
	Year            int          `json:"year"`
	PosterPath      string       `json:"posterPath"`
	Overview        string       `json:"overview"`
	State           string       `json:"state"`
	PublicNote      string       `json:"publicNote"`
	CreatedAt       time.Time    `json:"createdAt"`
	UpdatedAt       time.Time    `json:"updatedAt"`
	SubscriberCount int          `json:"subscriberCount"`
	Subscribed      bool         `json:"subscribed"`
	PrivateNote     string       `json:"privateNote,omitempty"`
	LibraryItemID   int64        `json:"-"`
	Fingerprint     string       `json:"-"`
	PolicyJSON      string       `json:"-"`
}
type HouseholdTarget struct {
	ID, RequestID, MediaItemID, EpisodeID int64
	ApprovalGeneration                    int64
	Season, Episode                       int
	State                                 string
	DownloadID                            int64
	DownloadGeneration                    string
}

type HouseholdPolicy struct {
	ItemID            int64       `json:"itemId"`
	Kind              MediaKind   `json:"kind"`
	TMDBID            int64       `json:"tmdbId"`
	IDs               ExternalIDs `json:"ids"`
	RootID            int64       `json:"rootId"`
	ProfileID         int64       `json:"profileId"`
	RootGeneration    int64       `json:"rootGeneration"`
	SharedRevision    int64       `json:"sharedRevision"`
	RootPath          string      `json:"rootPath"`
	RootIdentity      string      `json:"rootIdentity"`
	Path              string      `json:"path"`
	Reacquire         bool        `json:"reacquire"`
	OffloadOperations []string    `json:"offloadOperations,omitempty"`
}

// HouseholdOrigin is immutable publication authority for a newly admitted
// household download. Attaching to existing owner work never creates one.
type HouseholdOrigin struct {
	ItemID         int64     `json:"itemId"`
	Kind           MediaKind `json:"kind"`
	TMDBID         int64     `json:"tmdbId"`
	EpisodeIDs     []int64   `json:"episodeIds"`
	Path           string    `json:"path"`
	RootID         int64     `json:"rootId"`
	RootPath       string    `json:"rootPath"`
	RootIdentity   string    `json:"rootIdentity"`
	RootGeneration int64     `json:"rootGeneration"`
}
