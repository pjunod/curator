package acquisition

import (
	"context"
	"errors"
	"fmt"
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
// Three guards, all at the point of decision:
//   - the same release already in flight is never grabbed a second time
//   - the same release already delivered here, whose import failed and whose
//     payload is still retained, is never grabbed again automatically
//   - a wantable that has burned reGrabLimit grabs inside reGrabWindow stops
//     until the window rolls off — on every automatic path, RSS included
//
// Field report 2026-11-01: one obfuscated episode (payload named
// "321d96….mkv") failed import with "cannot determine episodes" and RSS
// grabbed the identical NZB thirteen times in five days. A failed import
// marks the row failed, which correctly lets a DIFFERENT release be tried —
// but nothing distinguished "this release's bytes are already on disk and
// failed to import" from "this release failed to download". Downloading the
// same bytes again cannot change an import verdict; the retained payload is
// already retried in place. RSS also never consulted the regrab cap, so the
// loop was paced only by the indexer, until its daily quota answered 429.

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
	rows, err := s.db.ListDownloadReservations(ctx)
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

// ErrReleaseAwaitingImport refuses an automatic grab of a release whose bytes
// are already here awaiting import.
var ErrReleaseAwaitingImport = errors.New("release already delivered and awaiting import")

// importFailedOnRetainedPayload reports whether a download's bytes arrived,
// monarr attempted to import them, the import failed, and the payload has not
// been removed. Only such a row proves that grabbing the same release again
// would fetch bytes that are already here.
func importFailedOnRetainedPayload(dl sqlite.Download) bool {
	if dl.State != "failed" || dl.PayloadRemoved || dl.Superseded {
		return false
	}
	for _, event := range dl.Handoff {
		if event.Step == stepImporting {
			return true
		}
	}
	return false
}

// deliveredAwaitingImport reports the id of a download for the same release
// whose payload is retained after a failed import, or 0.
func (s *Service) deliveredAwaitingImport(ctx context.Context, title string) int64 {
	want := normalizeRelease(title)
	if want == "" {
		return 0
	}
	rows, err := s.db.RetainedFailedDownloads(ctx)
	if err != nil {
		return 0
	}
	for _, dl := range rows {
		if normalizeRelease(dl.ReleaseTitle) == want && importFailedOnRetainedPayload(dl) {
			return dl.ID
		}
	}
	return 0
}

// skipCandidate reports whether automation must pass over a release: it is
// blocklisted, or its bytes are already here awaiting import. Passing over
// (rather than refusing at Grab) lets the selection fall through to a
// different release, which is the one that can actually help.
func (s *Service) skipCandidate(ctx context.Context, title, indexer string) bool {
	return s.isBlocklisted(ctx, title, indexer) || s.deliveredAwaitingImport(ctx, title) != 0
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
	rows, err := s.db.RecentFailedDownloads(ctx, time.Now().Add(-reGrabWindow))
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
		// A season covers episodes only for the same item and copy. Comparing
		// parsed coordinates avoids both `season:5:2` matching item 50 and a
		// primary pack suppressing a secondary copy.
		var gotItem, gotSeason, gotCopy, item, season, episode, copy int64
		if parseSeasonWantable(got, &gotItem, &gotSeason, &gotCopy) &&
			parseEpisodeWantable(id, &item, &season, &episode, &copy) &&
			gotItem == item && gotSeason == season && gotCopy == copy {
			return true
		}
	}
	return false
}

func parseSeasonWantable(id string, item, season, copy *int64) bool {
	*copy = 0
	n, _ := fmt.Sscanf(id, "season:%d:%d:c%d", item, season, copy)
	if n == 3 {
		return true
	}
	n, _ = fmt.Sscanf(id, "season:%d:%d", item, season)
	return n == 2
}

func parseEpisodeWantable(id string, item, season, episode, copy *int64) bool {
	*copy = 0
	n, _ := fmt.Sscanf(id, "episode:%d:%d:%d:c%d", item, season, episode, copy)
	if n == 4 {
		return true
	}
	n, _ = fmt.Sscanf(id, "episode:%d:%d:%d", item, season, episode)
	return n == 3
}
