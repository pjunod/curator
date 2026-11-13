package domain

import "time"

// Availability keeps catalogue membership, observed files and execution separate.
// Presence describes the primary copy and known aired regular episodes only.
// It never establishes playback readiness or permission to acquire/delete files.
type Availability struct {
	Catalog       string               `json:"catalog"`
	Presence      string               `json:"presence"`
	LibraryItemID int64                `json:"libraryItemId,omitempty"`
	CopyID        int64                `json:"copyId"`
	Expected      int                  `json:"expected"`
	Present       int                  `json:"present"`
	Offloaded     int                  `json:"offloaded"`
	Acquisition   string               `json:"acquisition"`
	CheckedAt     *time.Time           `json:"checkedAt,omitempty"`
	Reason        string               `json:"reason,omitempty"`
	Seasons       []SeasonAvailability `json:"seasons,omitempty"`
}

type SeasonAvailability struct {
	Number       int `json:"number"`
	Expected     int `json:"expected"`
	Present      int `json:"present"`
	Offloaded    int `json:"offloaded"`
	UnknownDates int `json:"unknownDates"`
}

type AvailabilityTarget struct {
	LibraryItemID int64
	Kind          MediaKind
	IDs           ExternalIDs
}
