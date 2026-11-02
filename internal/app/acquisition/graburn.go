package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
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
// monarr attempted to import them, the import failed for a reason fetching
// the same bytes again cannot change, and those bytes are still on disk.
//
// Excluded, because a fresh grab of the same release could help: a payload
// that was cut short (a transfer that stopped — deliberately not a verdict on
// the release, see truncatedPayload), and a payload that is no longer there.
// Excluded because an operator decided: a row parked or dismissed from
// Activity.
func importFailedOnRetainedPayload(dl sqlite.Download) bool {
	if dl.State != "failed" || dl.PayloadRemoved || dl.Superseded || dl.ParkedAt.UnixMilli() > 0 {
		return false
	}
	if strings.Contains(dl.Error, mediainfo.TruncatedReasonPrefix) {
		return false
	}
	attempted := false
	for _, event := range dl.Handoff {
		if event.Step == stepImporting {
			attempted = true
		}
	}
	if !attempted || !filepath.IsAbs(dl.ImportPath) {
		return false
	}
	_, err := os.Stat(dl.ImportPath)
	return err == nil
}

// deliveredAwaitingImport reports the id of a download for the same release
// whose payload is retained after a failed import, or 0.
func (s *Service) deliveredAwaitingImport(ctx context.Context, title string) int64 {
	want := normalizeRelease(title)
	if want == "" {
		return 0
	}
	rows, err := s.db.RetainedFailedDownloadsForRelease(ctx, want)
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
	if s.isBlocklisted(ctx, title, indexer) {
		return true
	}
	if id := s.deliveredAwaitingImport(ctx, title); id != 0 {
		s.log.Info("passing over a release already delivered and awaiting import",
			"release", title, "download", id)
		return true
	}
	return false
}

// skipCandidateFor is skipCandidate for a known title. A failed download is
// blocklisted by its exact release identity (indexer + GUID) whenever the
// indexer supplied one, and that identity table is per title, so the
// title/indexer check alone passed straight over it: the season planner
// re-chose the identical NZB seconds after it failed, dispatch refused it as
// blocklisted, and the season stalled (field report 2026-11-02, Dexter:
// Resurrection S02E01). Every path that knows the title must refuse what
// dispatch and Grab would refuse.
func (s *Service) skipCandidateFor(ctx context.Context, itemID int64, r ports.Release) bool {
	if s.skipCandidate(ctx, r.Title, r.Indexer) {
		return true
	}
	if itemID == 0 || r.GUID == "" || r.IndexerID == 0 {
		return false
	}
	_, blocked, err := s.db.ReleaseIdentityBlocklistReason(ctx, itemID, r.IndexerID, r.GUID)
	return err == nil && blocked
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
