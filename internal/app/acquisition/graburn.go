package acquisition

import (
	"context"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Not grabbing the same thing twice, and not grabbing forever.
//
// Field report 2026-07-31: five releases of one movie in ten hours, 336 GB,
// every one of them failing in the download client for the same reason; and
// two grabs of the *same NZB* six minutes apart. Both come from the same
// gap. `notInFlight` filters the wanted list at the automation entry points,
// but nothing checks at the moment of the grab — so two passes that overlap,
// a failure that re-searches instantly, and a manual grab all walk straight
// past it. And `handleFailure` re-searched with no counter and no delay, so
// a wantable whose every release fails just cycles the indexer's top five.
//
// Two guards, both at the point of decision:
//   - the same release already in flight is never grabbed a second time
//   - a wantable that has burned reGrabLimit grabs inside reGrabWindow stops
//     until the window rolls off

const (
	// reGrabLimit is how many failed grabs one wantable may spend inside
	// reGrabWindow before automation stops replacing them.
	reGrabLimit = 3
	// reGrabWindow is the rolling window the limit applies over.
	reGrabWindow = 12 * time.Hour
)

// HistoryRegrabCapped records a re-search that did NOT happen, because a
// silent stop is indistinguishable from a bug.
const HistoryRegrabCapped = "regrab_capped"

// alreadyInFlight reports the id of an active download for the same
// release, or 0. Titles are normalized the same way matchStatus normalizes
// them, so punctuation from an indexer cannot smuggle a duplicate past it.
func (s *Service) alreadyInFlight(ctx context.Context, req GrabRequest) int64 {
	rows, err := s.db.ListActiveDownloads(ctx)
	if err != nil {
		return 0
	}
	want := normalizeRelease(req.Title)
	if want == "" {
		return 0
	}
	for _, dl := range rows {
		if normalizeRelease(dl.ReleaseTitle) == want {
			return dl.ID
		}
	}
	return 0
}

// normalizeRelease folds the separators indexers disagree about, so
// "True.Lies-HDS" and "True Lies HDS" are one release.
func normalizeRelease(s string) string {
	return strings.TrimSpace(strings.ToLower(
		strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(s)))
}

// regrabCapped reports whether a wantable has burned its grabs for now, and
// how many it has spent. Counted from the downloads table rather than a
// counter column: the rows ARE the record, and a count derived from them
// cannot drift from what actually happened.
func (s *Service) regrabCapped(ctx context.Context, w domain.Wantable) (bool, int) {
	rows, err := s.db.ListRecentDownloads(ctx)
	if err != nil {
		return false, 0
	}
	id := string(w.ID())
	cutoff := time.Now().Add(-reGrabWindow)
	spent := 0
	for _, dl := range rows {
		if dl.State != "failed" || dl.AddedAt.Before(cutoff) {
			continue
		}
		if wantableOnRow(dl, id) {
			spent++
		}
	}
	return spent >= reGrabLimit, spent
}

// wantableOnRow reports whether a download row was grabbed for this
// wantable id — directly, or as the season pack that covers an episode.
func wantableOnRow(dl sqlite.Download, id string) bool {
	for _, got := range dl.WantableIDs {
		if got == id {
			return true
		}
		// "season:5:2" covers "episode:5:2:7".
		if strings.HasPrefix(got, "season:") && strings.HasPrefix(id, "episode:") {
			if strings.HasPrefix(id, "episode:"+strings.TrimPrefix(got, "season:")+":") {
				return true
			}
		}
	}
	return false
}
