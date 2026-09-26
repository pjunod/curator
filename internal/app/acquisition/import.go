package acquisition

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/naming"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/probe"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// FileOutcome is what happened to one file in an import, including — above
// all — why it did not land.
//
// This exists because "no files imported from <path>" was the entire story a
// user got when an import declined every file. The reasons were computed, they
// were even logged, and then they were dropped on the floor before reaching the
// one person who needed them. A refusal without a reason is indistinguishable
// from a bug.
type FileOutcome struct {
	Name     string `json:"name"`
	Imported bool   `json:"imported"`
	Upgrade  bool   `json:"upgrade,omitempty"`
	Quality  string `json:"quality,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Retryable marks a file that failed on the environment (a full disk,
	// a mount that went away) rather than on its own merits — worth trying
	// again unchanged, unlike a quality decision.
	Retryable bool `json:"retryable,omitempty"`
	// Path is where the file landed — absolute, as Monarr sees it. Empty
	// when the file was skipped.
	Path string `json:"path,omitempty"`
}

// ImportResult is the outcome of one payload.
type ImportResult struct {
	Imported int
	Upgraded bool
	Files    []FileOutcome
	// Blocked is set when the import stopped on the environment rather
	// than finishing: the remaining files were never attempted, and the
	// download must not be treated as done.
	Blocked error
}

// ErrImportBlocked marks an import that stopped on something about the
// machine — a full volume, a mount that went away — rather than on the
// payload. The retry sweep looks for exactly this.
var ErrImportBlocked = errors.New("import stopped")

// HistoryImportBlocked records a half-finished import, so "where did the
// second half of that season go" has an answer that is not the server log.
const HistoryImportBlocked = "import_blocked"

// importedEvent assembles the ImportCompleted for one finished payload.
//
// Only files that actually landed are listed. A skipped file has no path,
// and a consumer told to index one would either 404 or — worse, if the
// rejected file is still sitting in the download folder — index a copy of it
// from outside the library.
func importedEvent(item domain.MediaItem, dl sqlite.Download, r ImportResult) ImportCompleted {
	paths := make([]string, 0, r.Imported)
	var dirs []string
	seen := map[string]bool{}
	for _, f := range r.Files {
		if !f.Imported || f.Path == "" {
			continue
		}
		paths = append(paths, f.Path)
		// Unique, in first-seen order. A season pack is one directory and a
		// dozen files: one request that names the directory beats a dozen
		// that name the same directory twelve times over, and stable order
		// keeps the trace readable across runs.
		if dir := filepath.Dir(f.Path); !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	event := ImportCompleted{
		MediaItemID:   item.ID,
		Release:       dl.ReleaseTitle,
		Files:         r.Imported,
		Upgrade:       r.Upgraded,
		Paths:         paths,
		Dirs:          dirs,
		MediaItemKind: string(item.Kind),
		Title:         item.Title,
		TmdbID:        item.IDs.TMDB,
		ImdbID:        item.IDs.IMDB,
		DownloadID:    dl.ID,
		Transfer:      dl.Transfer,
	}
	if item.Kind == domain.KindBook {
		event.BookTitle = curatorBookText(item.Title)
		event.BookAuthor = curatorBookText(item.Author)
		event.BookMedium = importedBookMedium(item, dl)
		event.BookWorkID = curatorBookWorkID(item)
		event.BookEditionID = fmt.Sprintf("curator:item:%d:%s", item.ID, event.BookMedium)
		event.BookCoverURL = openLibraryCoverURL(item.PosterPath)
		// An incomplete legacy row should still trigger the same targeted scan,
		// but not attach a malformed `book` object that Cinema must reject.
		if event.BookMedium == "" {
			event.BookWorkID = ""
			event.BookEditionID = ""
		}
	}
	return event
}

func curatorBookText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return ""
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return value
}

func importedBookMedium(item domain.MediaItem, dl sqlite.Download) string {
	if dl.CopyID == 0 && quality.ValidBookType(item.BookType) {
		return string(item.BookType)
	}
	for _, copy := range item.Copies {
		if copy.ID == dl.CopyID && quality.ValidBookType(copy.BookType) {
			return string(copy.BookType)
		}
	}
	medium := quality.BookTypeForSource(dl.Quality.Source)
	if quality.ValidBookType(medium) {
		return string(medium)
	}
	return ""
}

func curatorBookWorkID(item domain.MediaItem) string {
	olid := strings.TrimSpace(item.IDs.OLID)
	if isOpenLibraryWorkID(olid) {
		return "curator:openlibrary:" + olid
	}
	return fmt.Sprintf("curator:item:%d", item.ID)
}

func isOpenLibraryWorkID(value string) bool {
	if len(value) < 4 || !strings.HasPrefix(value, "OL") || !strings.HasSuffix(value, "W") {
		return false
	}
	for _, r := range value[2 : len(value)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func openLibraryCoverURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 2_048 {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() != "covers.openlibrary.org" ||
		parsed.Port() != "" || parsed.User != nil {
		return ""
	}
	return value
}

// placement is what importing one file produced: where it landed, and
// whether it replaced something. The path matters beyond bookkeeping — it is
// what a media server is told to index, and "the folder the item lives in"
// is not a good enough answer when a season pack drops six files into a
// library that is otherwise unchanged.
type placement struct {
	Path    string
	Upgrade bool
}

// Skipped returns the files that did not land, in payload order.
func (r ImportResult) Skipped() []FileOutcome {
	var out []FileOutcome
	for _, f := range r.Files {
		if !f.Imported {
			out = append(out, f)
		}
	}
	return out
}

// reasons renders the per-file refusals as one line, capped so a 24-file
// season pack does not produce a wall of text. Silent truncation reads as
// "that was all of them", so the remainder is counted out loud.
func (r ImportResult) reasons(limit int) string {
	skipped := r.Skipped()
	if len(skipped) == 0 {
		return ""
	}
	parts := make([]string, 0, limit+1)
	for i, f := range skipped {
		if i == limit {
			parts = append(parts, fmt.Sprintf("and %d more", len(skipped)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s: %s", f.Name, f.Reason))
	}
	return strings.Join(parts, "; ")
}

// importDownload moves a completed payload into the library: parse each
// video file, map it to wantables, apply per-file upgrade decisions,
// hardlink-or-copy into the Renamer layout, link MediaFile rows, and clean
// up replaced files (blueprint §5.1 grab → import).
//
// manual marks an import the user asked for by hand. Automation is gated by
// the profile — that is the whole point of a profile — but a person who
// pointed at a folder and pressed Import has already made the decision, and
// second-guessing them with "does not improve on" is the same mistake as
// gating a manual grab (which monarr has never done).
func (s *Service) importDownload(ctx context.Context, dl sqlite.Download, savePath string, manual bool) (ImportResult, error) {
	return s.importDownloadFiles(ctx, dl, savePath, nil, manual)
}

// importDownloadFiles is the selected-file form of importDownload. A nil
// selection preserves the automatic/legacy whole-payload behavior; a
// present selection imports exactly what the manual-import scan offered.
func (s *Service) importDownloadFiles(ctx context.Context, dl sqlite.Download, savePath string, selected []string, manual bool) (ImportResult, error) {
	if ctx.Value(targetHeldKey{}) != true {
		s.importTargetMu.Lock()
		defer s.importTargetMu.Unlock()
		ctx = context.WithValue(ctx, targetHeldKey{}, true)
	}

	item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		return ImportResult{}, err
	}

	// The import scope: which copy this grab was for decides the profile,
	// the destination folder, and which existing files count as "current".
	scope := importScope{Dest: item.Path, ProfileID: item.QualityProfileID,
		Release: dl.ReleaseTitle, Indexer: dl.Indexer, Attempt: fmt.Sprintf("%d:%d:%s", dl.ID, dl.AddedAt.UnixNano(), dl.Handle)}
	if dl.CopyID != 0 {
		cp, err := s.db.GetMediaCopy(ctx, dl.MediaItemID, dl.CopyID)
		if err != nil {
			return ImportResult{}, fmt.Errorf("copy %d vanished: %w", dl.CopyID, err)
		}
		scope.CopyID = cp.ID
		scope.ProfileID = cp.QualityProfileID
		if cp.Path != "" {
			scope.Dest = cp.Path
		}
	}
	if scope.Dest == "" {
		return ImportResult{}, ErrNoLibraryFolder
	}

	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok {
		// These exact paths were content-probed and digest-bound at admission.
		isMedia = func(path string) bool {
			return recovery.Files[path] != "" || recovery.Files[filepath.Join(savePath, path)] != ""
		}
	}
	var videos []string
	if selected == nil {
		videos, err = collectFiles(savePath, isMedia)
	} else {
		if len(selected) == 0 {
			return ImportResult{}, fmt.Errorf("no files selected")
		}
		videos, err = selectedFiles(savePath, selected, isMedia)
	}
	if err != nil {
		return ImportResult{}, err
	}
	if len(videos) == 0 {
		return ImportResult{}, fmt.Errorf("no media files in %s", savePath)
	}
	// Multipart audiobook names are positional. WalkDir is lexical already,
	// but manual selection order is a UI detail and must not change the track
	// numbers assigned to the same payload on a retry.
	if item.Kind == domain.KindBook {
		sort.Strings(videos)
	}
	// A movie is one file. When a payload offers several, take the biggest.
	//
	// Only one of them can win — the others are declined as "does not improve
	// on" whatever landed first — so the only question is which one gets to be
	// first, and alphabetical order is a terrible way to answer it. A payload
	// carrying the feature plus a decoy (usenet spam does this; so does the
	// occasional stray extra) would hand the library whichever one sorted
	// earlier, and "AAA-something.mkv" sorts earlier than most real releases.
	//
	// Season packs and multi-episode files are untouched: they are matched per
	// episode, so every file in them has its own slot to win.
	if item.Kind == domain.KindMovie && len(videos) > 1 {
		sort.SliceStable(videos, func(i, j int) bool { return sizeOf(videos[i]) > sizeOf(videos[j]) })
		s.log.Info("import: payload has more than one video; taking the largest",
			"release", dl.ReleaseTitle, "files", len(videos),
			"chosen", filepath.Base(videos[0]))
	}

	// Did we get what was advertised?
	//
	// This is the cheapest question in the whole pipeline and it was never
	// asked. The indexer states a size at grab time and monarr stores it on
	// the download row, and then nothing ever compares it to the bytes that
	// arrived. Without that comparison "the release was a 500 MB fake" and
	// "a 13 GB release arrived 500 MB short" are indistinguishable from the
	// library, and they call for opposite responses: blocklist the release,
	// or go look at the download client.
	//
	// It does not refuse the import. A short payload is a fact about the
	// transfer, not a verdict on the release, and refusing here would strand
	// legitimate imports whose indexer simply lies about size. The probe's
	// plausibility rules already stop a runt file from satisfying the item;
	// this exists to say WHY in terms a person can act on.
	s.checkDelivered(ctx, dl, item.ID, videos)

	epStates, err := s.episodeStates(ctx, item, scope.CopyID)
	if err != nil {
		return ImportResult{}, err
	}
	profile, err := s.db.GetProfile(ctx, scope.ProfileID)
	if err != nil {
		return ImportResult{}, err
	}

	// Book payloads are checked as a set before any bytes move. In particular,
	// a multipart audiobook is one edition: accepting track 01 and only then
	// discovering track 02 is the wrong family would leave a curated folder
	// that can never be complete.
	var bookQualities []quality.Quality
	var audioPlan *bookImportPlan
	if item.Kind == domain.KindBook {
		bookQualities = make([]quality.Quality, len(videos))
		outcomes := make([]FileOutcome, 0, len(videos))
		for i, src := range videos {
			q := quality.Quality{Source: quality.Source(filename.BookQualitySource(src))}
			bookQualities[i] = q
			if quality.BookTypeForSource(q.Source) != quality.BookTypeForSource(profile.Target.Source) {
				outcomes = append(outcomes, FileOutcome{
					Name: filepath.Base(src), Quality: q.Display(),
					Reason: fmt.Sprintf("%s is not accepted by the %s profile", q.Display(), profile.Name),
				})
				continue
			}
			if !manual && !profile.Acceptable(q) {
				outcomes = append(outcomes, FileOutcome{
					Name: filepath.Base(src), Quality: q.Display(),
					Reason: fmt.Sprintf("%s is below the floor for profile %q", q.Display(), profile.Name),
				})
			}
		}
		if len(outcomes) > 0 {
			result := ImportResult{Files: outcomes}
			return result, fmt.Errorf("no files imported from %s — %s", savePath, result.reasons(5))
		}
		if quality.BookTypeForSource(profile.Target.Source) == quality.BookTypeAudiobook && len(videos) > 1 {
			audioPlan, err = s.planBookImport(ctx, item, scope, profile, bookQualities, manual)
			if err != nil {
				outcomes = outcomes[:0]
				for i, src := range videos {
					outcomes = append(outcomes, FileOutcome{Name: filepath.Base(src), Quality: bookQualities[i].Display(), Reason: err.Error()})
				}
				return ImportResult{Files: outcomes}, fmt.Errorf("no files imported from %s — %v", savePath, err)
			}
		}
	}

	result := ImportResult{Files: make([]FileOutcome, 0, len(videos))}
	for fileIndex, src := range videos {
		p := parser.Parse(filepath.Base(src))
		if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok {
			if target, ok := recovery.EpisodeTargets[src]; ok {
				p.Season = target.Season
				p.Episodes = target.Episodes
			}
		}
		q := p.Quality
		if item.Kind == domain.KindBook {
			q = bookQualities[fileIndex]
		} else if q.Source == quality.SourceUnknown && q.Resolution == 0 {
			// Single files often carry the quality only on the release name.
			q = dl.Quality
		}

		outcome := FileOutcome{Name: filepath.Base(src), Quality: q.Display()}
		// Refuse a file that is provably incomplete, before it is placed.
		//
		// This is the one refusal in the import path that is not a judgement
		// call. Everywhere else monarr imports and lets the library sort it
		// out, because a file that is worse than promised is still a file. A
		// cut-short file is not: the container states its own finished length
		// and the bytes are not there, so what would land is a stump.
		//
		// Placing it anyway is worse than doing nothing. The item goes back to
		// wanted (the plausibility rules see to that), the next backlog pass
		// grabs again, gets another stump, replaces the first, and monarr
		// spends the night rediscovering the same broken transfer. Failing the
		// import stops that: the download is surfaced as failed, with the
		// numbers, and it is retryable the moment the underlying problem is
		// fixed.
		//
		// Deliberately NOT a blocklist. The release is fine — 500 MB of a
		// 60 GB remux is a transfer that stopped, and punishing the release
		// for it would burn a good one and send monarr after a worse copy.
		if why, cut := truncatedPayload(src); cut {
			outcome.Reason = why
			s.log.Warn("import: refusing a file that is cut short",
				"release", dl.ReleaseTitle, "file", outcome.Name, "why", why)
			_ = s.db.AddHistory(ctx, HistoryShortDelivery, item.ID, dl.ReleaseTitle,
				map[string]any{"file": outcome.Name, "reason": why})
			result.Files = append(result.Files, outcome)
			continue
		}
		var err error
		var put placement
		switch item.Kind {
		case domain.KindMovie:
			put, err = s.importMovieFile(ctx, item, scope, profile, src, q, dl.ReleaseTitle, manual)
		case domain.KindBook:
			put, err = s.importBookFile(ctx, item, scope, profile, src, q, manual,
				fileIndex, len(videos), audioPlan)
		default:
			put, err = s.importEpisodeFile(ctx, item, scope, profile, epStates, src, p, q, dl.ReleaseTitle, manual)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return result, err
			}
			outcome.Reason = err.Error()
			if retryableIO(err) {
				// Not a verdict on the file — a condition of the machine.
				// Stop here: every remaining file would fail the same way,
				// and half a season pack imported with no error anywhere is
				// how a library ends up listing episodes it is sitting on.
				outcome.Retryable = true
				result.Files = append(result.Files, outcome)
				result.Blocked = err
				s.log.Error("import: stopped — the destination cannot take the files",
					"release", dl.ReleaseTitle, "file", outcome.Name, "err", err,
					"placed", result.Imported, "of", len(videos))
				break
			}
			s.log.Warn("import: file skipped", "file", outcome.Name, "reason", err)
		} else {
			outcome.Imported, outcome.Upgrade = true, put.Upgrade
			outcome.Path = put.Path
			result.Imported++
			result.Upgraded = result.Upgraded || put.Upgrade
		}
		result.Files = append(result.Files, outcome)
	}
	if audioPlan != nil && result.Blocked == nil && result.Imported == len(videos) && audioPlan.Upgrade {
		keep := make([]string, 0, len(result.Files))
		for _, outcome := range result.Files {
			if outcome.Imported {
				keep = append(keep, outcome.Path)
			}
		}
		s.removeExistingFiles(ctx, item, scope.CopyID, nil, keep...)
	}
	// The environment first: an import that stopped because the destination
	// could not take the bytes is a FAILED import, whether it placed none
	// of the files or all but one. Files that did land stay in the library
	// and are recorded here; the download is surfaced as failed so the
	// retry sweep finishes it once there is room, and so the payload is not
	// cleaned up out from under the half that is missing. Checked BEFORE
	// "no files imported", or a payload whose very first file hit a full
	// disk reads as an unparseable release and is never retried.
	if result.Blocked != nil {
		_ = s.db.AddHistory(ctx, HistoryImportBlocked, item.ID, dl.ReleaseTitle,
			map[string]any{"placed": result.Imported, "of": len(videos),
				"reason": result.Blocked.Error()})
		s.log.Info("imported (partial)", "item", item.Title, "files", result.Imported,
			"remaining", len(videos)-result.Imported)
		return result, fmt.Errorf("%w: %d of %d files placed from %s before it stopped: %v",
			ErrImportBlocked, result.Imported, len(videos), savePath, result.Blocked)
	}
	if result.Imported == 0 {
		// Say WHY, per file. This is the message a user actually reads when
		// an import does nothing, and "no files imported" alone told them
		// only that something went wrong somewhere.
		return result, fmt.Errorf("no files imported from %s — %s", savePath, result.reasons(5))
	}

	_ = s.db.AddHistory(ctx, "imported", item.ID, dl.ReleaseTitle,
		map[string]any{"files": result.Imported, "upgrade": result.Upgraded})
	s.publish(importedEvent(item, dl, result))
	s.log.Info("imported", "item", item.Title, "files", result.Imported,
		"skipped", len(result.Skipped()))
	return result, nil
}

// selectedFiles validates the browser's selection at the trust boundary.
// The scan result can go stale and an API caller can submit anything, so each
// entry must still be a real media file beneath the scanned path.
func selectedFiles(root string, selected []string, isMedia func(string) bool) ([]string, error) {
	root = filepath.Clean(root)
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("payload missing: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("payload missing: %w", err)
	}
	out := make([]string, 0, len(selected))
	seen := map[string]bool{}
	for _, raw := range selected {
		p := filepath.Clean(raw)
		resolved, resolveErr := filepath.EvalSymlinks(p)
		outside := resolveErr != nil
		if rootInfo.IsDir() && !outside {
			rel, relErr := filepath.Rel(resolvedRoot, resolved)
			outside = relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
		} else if !rootInfo.IsDir() && !outside {
			outside = resolved != resolvedRoot
		}
		if outside {
			return nil, fmt.Errorf("selected file is outside %s: %s", root, raw)
		}
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || !isMedia(p) ||
			strings.Contains(strings.ToLower(filepath.Base(p)), "sample") {
			return nil, fmt.Errorf("selected file is not importable: %s", raw)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// truncatedPayload reports whether a file declares more bytes than it has, and
// says so in the user's terms.
//
// Reads the header only (the prober never reads a whole file), so this costs
// one bounded read per imported file — paid once at import, against the cost
// of a stump in the library and a search loop that never settles.
//
// Anything it cannot judge passes. A container monarr does not deep-parse, a
// muxer that declared no length, an unreadable file: none of those are proof
// of anything, and this refusal is only for the case that is.
func truncatedPayload(src string) (string, bool) {
	if !filename.IsVideo(src) {
		return "", false // a book's bytes are its own business
	}
	info, _ := probe.File(src)
	if !info.Truncated() {
		return "", false
	}
	why, _ := mediainfo.Implausible(info)
	return why.Reason, true
}

// HistoryShortDelivery records a payload that arrived materially smaller than
// the indexer advertised.
const HistoryShortDelivery = "short_delivery"

// deliveredFloor is the fraction of the advertised size a payload has to reach
// before monarr stops caring.
//
// Half, because the advertised number is honestly approximate: a usenet post's
// size includes par2 and rar overhead the extracted media does not carry, and
// some indexers round or report the whole posting rather than the file. Real
// overhead runs 3-15%. Anything that arrives under half of what was promised
// is not overhead, it is a different thing than the one that was offered.
const deliveredFloor = 0.5

// checkDelivered compares the bytes that arrived against the bytes that were
// advertised, and says so loudly when they disagree.
func (s *Service) checkDelivered(ctx context.Context, dl sqlite.Download, itemID int64, files []string) {
	if dl.Size <= 0 || len(files) == 0 {
		return // nothing was advertised, or nothing arrived to compare
	}
	var delivered int64
	for _, f := range files {
		delivered += sizeOf(f)
	}
	if delivered <= 0 || float64(delivered) >= float64(dl.Size)*deliveredFloor {
		return
	}
	pct := float64(delivered) / float64(dl.Size) * 100
	s.log.Warn("import: payload is far smaller than the indexer advertised",
		"release", dl.ReleaseTitle, "indexer", dl.Indexer,
		"advertised", dl.Size, "delivered", delivered,
		"percent", fmt.Sprintf("%.0f%%", pct),
		"note", "check the download client before blaming the release")
	_ = s.db.AddHistory(ctx, HistoryShortDelivery, itemID, dl.ReleaseTitle, map[string]any{
		"advertised": dl.Size,
		"delivered":  delivered,
		"percent":    fmt.Sprintf("%.0f%%", pct),
		"files":      len(files),
		"indexer":    dl.Indexer,
	})
}

func collectFiles(savePath string, isMedia func(string) bool) ([]string, error) {
	info, err := os.Stat(savePath)
	if err != nil {
		return nil, fmt.Errorf("payload missing: %w", err)
	}
	if !info.IsDir() {
		if isMedia(savePath) {
			return []string{savePath}, nil
		}
		return nil, nil
	}
	var out []string
	err = filepath.WalkDir(savePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isMedia(path) {
			return err
		}
		// Skip samples.
		if strings.Contains(strings.ToLower(filepath.Base(path)), "sample") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

// importScope pins an import to one copy of the item: its id (0 =
// primary), its destination folder, its quality profile, and what produced
// the payload.
type importScope struct {
	Attempt      string
	DeferCleanup bool
	CopyID       int64
	Dest         string
	ProfileID    int64
	// Release and Indexer name the thing being imported. They ride along here
	// rather than as two more parameters on three near-identical signatures,
	// and they exist so each placed file can remember its own origin —
	// without that, "this file is bad, blocklist it" has nothing to blocklist
	// once the download row leaves the queue.
	Release string
	Indexer string
}

func (s *Service) importMovieFile(ctx context.Context, item domain.MediaItem, scope importScope, profile quality.Profile, src string, q quality.Quality, releaseTitle string, manual bool) (placement, error) {
	state, err := s.db.DiskStateForItem(ctx, item.ID, scope.CopyID)
	if err != nil {
		return placement{}, err
	}
	upgrade := false
	if state.Best != nil {
		upgrade = profile.Upgrade(q, *state.Best, state.SourceVerified)
		if !upgrade && !manual {
			return placement{}, fmt.Errorf("%s does not improve on the %s already here (profile %q)",
				q.Display(), state.Best.Display(), profile.Name)
		}
		// A manual import proceeds either way, but only REPLACES when the new
		// file actually outranks what is there. Deleting a better file because
		// somebody imported a worse one is not a thing to do silently.
		upgrade = upgrade || quality.Better(q, *state.Best)
	}
	_ = profile

	dest := movieDestination(scope.Dest, item, src, q)
	if r, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok {
		dest = r.Destinations[src]
	}
	_, err = s.commitPlacement(ctx, item, scope, src, dest, q, nil, upgrade)
	if err != nil {
		return placement{}, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, nil, dest)
	}
	return placement{Path: dest, Upgrade: upgrade}, nil
}

// importBookFile places one book file into <library>/<Author>/<Title>/ as
// "Title - Author.ext" (Calibre-friendly, ADR 0006), with the same
// upgrade-or-reject semantics as movies.
type bookImportPlan struct {
	Upgrade bool
}

func (s *Service) planBookImport(ctx context.Context, item domain.MediaItem, scope importScope, profile quality.Profile, qualities []quality.Quality, manual bool) (*bookImportPlan, error) {
	state, err := s.db.DiskStateForItem(ctx, item.ID, scope.CopyID)
	if err != nil {
		return nil, err
	}
	worst := qualities[0]
	for _, q := range qualities[1:] {
		if quality.Better(worst, q) {
			worst = q
		}
	}
	plan := &bookImportPlan{}
	if state.Best == nil {
		return plan, nil
	}
	plan.Upgrade = profile.Upgrade(worst, *state.Best, state.SourceVerified)
	if !plan.Upgrade && !manual {
		return nil, fmt.Errorf("%s audiobook does not improve on the %s already here (profile %q)",
			worst.Display(), state.Best.Display(), profile.Name)
	}
	plan.Upgrade = plan.Upgrade || quality.Better(worst, *state.Best)
	return plan, nil
}

func (s *Service) importBookFile(ctx context.Context, item domain.MediaItem, scope importScope, profile quality.Profile, src string, q quality.Quality, manual bool, part, total int, plan *bookImportPlan) (placement, error) {
	if plan != nil {
		scope.DeferCleanup = true
		base := naming.BookFileName(item.Author, item.Title)
		if total > 1 {
			base += fmt.Sprintf(" - %03d", part+1)
		}
		dest := filepath.Join(scope.Dest, base+strings.ToLower(filepath.Ext(src)))
		_, err := s.commitPlacement(ctx, item, scope, src, dest, q, nil, plan.Upgrade)
		if err != nil {
			return placement{}, err
		}
		return placement{Path: dest, Upgrade: plan.Upgrade}, nil
	}
	state, err := s.db.DiskStateForItem(ctx, item.ID, scope.CopyID)
	if err != nil {
		return placement{}, err
	}
	upgrade := false
	if state.Best != nil {
		upgrade = profile.Upgrade(q, *state.Best, state.SourceVerified)
		if !upgrade && !manual {
			return placement{}, fmt.Errorf("%s does not improve on the %s already here (profile %q)",
				q.Display(), state.Best.Display(), profile.Name)
		}
		// A manual import proceeds either way, but only REPLACES when the new
		// file actually outranks what is there. Deleting a better file because
		// somebody imported a worse one is not a thing to do silently.
		upgrade = upgrade || quality.Better(q, *state.Best)
	}

	dest := filepath.Join(scope.Dest,
		naming.BookFileName(item.Author, item.Title)+strings.ToLower(filepath.Ext(src)))
	_, err = s.commitPlacement(ctx, item, scope, src, dest, q, nil, upgrade)
	if err != nil {
		return placement{}, err
	}
	// Books are not probed: the extension IS the format (ADR 0006), so the
	// provenance is the filename and there is nothing to measure.
	return placement{Path: dest, Upgrade: upgrade}, nil
}

func (s *Service) importEpisodeFile(ctx context.Context, item domain.MediaItem, scope importScope, profile quality.Profile, epStates map[int64]episodeState, src string, p parser.Parsed, q quality.Quality, releaseTitle string, manual bool) (placement, error) {
	season, eps := p.Season, p.Episodes
	if len(eps) == 0 {
		if fx, ok := filename.Extract(filepath.Base(src)); ok {
			season, eps = fx.Season, fx.Episodes
		}
	}
	if len(eps) == 0 || season < 0 {
		return placement{}, fmt.Errorf("cannot determine episodes from %q", filepath.Base(src))
	}

	// Resolve episode ids + titles; per-file upgrade check against the
	// WORST current quality among covered episodes.
	var epIDs []int64
	var epTitles []string
	var worst *quality.Quality
	worstVerified := true
	missing := false
	for _, epNum := range eps {
		epID, err := s.db.GetEpisodeID(ctx, item.ID, season, epNum)
		if err != nil {
			continue // unknown episode; still import the known ones
		}
		epIDs = append(epIDs, epID)
		for _, se := range item.Seasons {
			if se.Number != season {
				continue
			}
			for _, e := range se.Episodes {
				if e.ID == epID {
					epTitles = append(epTitles, e.Title)
				}
			}
		}
		st := epStates[epID]
		if st.Have == nil {
			// Nothing known here. If a file exists it is unverified rather
			// than missing, and either way this import is not an upgrade
			// over something we cannot compare against.
			missing = true
		} else if worst == nil || quality.Better(*worst, *st.Have) {
			worst = st.Have
			worstVerified = st.Verified
		}
	}
	if len(epIDs) == 0 {
		return placement{}, fmt.Errorf("no known episodes for S%02d %v", season, eps)
	}
	upgrade := false
	if !missing && worst != nil {
		upgrade = profile.Upgrade(q, *worst, worstVerified)
		if !upgrade && !manual {
			return placement{}, fmt.Errorf("%s does not improve on the %s already here (profile %q)",
				q.Display(), worst.Display(), profile.Name)
		}
		upgrade = upgrade || quality.Better(q, *worst)
	}

	title := ""
	if len(epTitles) > 0 {
		title = epTitles[0]
	}
	dest := episodeDestination(scope.Dest, item, src, q, season, eps, title)
	if r, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok {
		dest = r.Destinations[src]
	}
	_, err := s.commitPlacement(ctx, item, scope, src, dest, q, epIDs, upgrade)
	if err != nil {
		return placement{}, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, epIDs, dest)
	}
	return placement{Path: dest, Upgrade: upgrade}, nil
}

func movieDestination(root string, item domain.MediaItem, src string, q quality.Quality) string {
	name := naming.Render(naming.MovieFileTemplate, map[string]string{"Movie Title": item.Title, "Release Year": strconv.Itoa(item.Year), "Quality Full": q.Display()})
	return filepath.Join(root, naming.SafeFileName(name)+filepath.Ext(src))
}
func episodeDestination(root string, item domain.MediaItem, src string, q quality.Quality, season int, eps []int, title string) string {
	token := fmt.Sprintf("S%02dE%02d", season, eps[0])
	if len(eps) > 1 {
		token += fmt.Sprintf("-E%02d", eps[len(eps)-1])
	}
	base := fmt.Sprintf("%s - %s - %s [%s]", item.Title, token, title, q.Display())
	return filepath.Join(root, naming.Render(naming.SeasonFolderTemplate, map[string]string{"season": strconv.Itoa(season)}), naming.SafeFileName(base)+filepath.Ext(src))
}

// removeExistingFiles deletes replaced files (rows + disk) for the target
// scope: all of ONE COPY's item files for movies, or that copy's files
// linked to the given episodes. Other copies' files are never touched —
// upgrading the 4K primary must not delete the 720p copy.
func (s *Service) removeExistingFiles(ctx context.Context, item domain.MediaItem, copyID int64, episodeIDs []int64, keep ...string) {
	files, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return
	}
	covered := map[int64]bool{}
	kept := map[string]bool{}
	for _, path := range keep {
		kept[path] = true
	}
	for _, id := range episodeIDs {
		covered[id] = true
	}
	for _, f := range files {
		if kept[f.Path] || f.CopyID != copyID {
			continue
		}
		if episodeIDs != nil {
			hit := false
			for _, id := range f.EpisodeIDs {
				if covered[id] {
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		if err := rejectSymlinks(f.Path, true); err != nil {
			continue
		}
		if rmErr := os.Remove(f.Path); rmErr != nil && !os.IsNotExist(rmErr) {
			s.log.Warn("import: could not remove replaced file; keeping its record", "path", f.Path, "err", rmErr)
			continue
		}
		if err := syncPath(filepath.Dir(f.Path)); err != nil {
			continue
		}
		var deleteErr error
		if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok {
			deleteErr = s.db.DeleteRecoverySuperseded(ctx, f.ID, recovery.ImportID)
		} else {
			deleteErr = s.db.DeleteFile(ctx, f.ID)
		}
		if deleteErr != nil {
			s.log.Warn("import: removed file metadata pending", "path", f.Path, "err", deleteErr)
		}
	}
}

// place hardlinks src to dest, falling back to copy across filesystems, and
// sets the result's permissions explicitly.
//
// The explicit chmod is not decoration. The copy path uses os.CreateTemp,
// which creates files mode 0600 — so every cross-filesystem import produced a
// file that no media server could open and no other user could even probe,
// while reporting complete success. The hardlink path is the same hazard
// wearing different clothes: the file keeps whatever mode the download client
// gave it. Neither is a mode monarr chose, so monarr now chooses.
//
// A hardlink shares one inode with the source, so the chmod is visible to the
// download client's copy too. That is the intended trade and the same one
// upstream makes: a seeding file that is readable is strictly better than a
// library file that is not.
// placeFile is `place`, behind a seam. The one thing a test cannot
// otherwise produce on demand is a destination that refuses the bytes —
// running as root makes a permission-denied fixture pass, and nobody can
// fill a volume in a unit test — and "what happens when the copy fails
// halfway" is the whole defect this package now guards against.
var placeFile = placeContext

func place(src, dest string, onBytes func(done, total int64)) error {
	return placeContext(context.Background(), src, dest, onBytes)
}

func placeContext(ctx context.Context, src, dest string, onBytes func(done, total int64)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(dest)
	// Record which folders do not exist yet, so the chmod below touches only
	// the ones monarr is about to create. Walking up and fixing whatever it
	// finds would eventually chmod the root folder — or the filesystem root —
	// which is not monarr's to change.
	created := missingDirs(dir)
	if err := os.MkdirAll(dir, dirMode()); err != nil {
		return err
	}
	// MkdirAll is subject to the process umask; chmod is not. Under umask 077
	// the folder would be 0700 and the file inside unreachable however
	// correct its own mode.
	for _, d := range created {
		_ = applyMode(d, dirMode())
	}
	// A file already at the destination is only "already imported" if it is
	// the SAME file. Anything else sharing that name is a stranger.
	//
	// This used to be a bare existence check, and it quietly defeated every
	// same-quality replacement in the product. The destination name is
	// rendered from the quality, so re-grabbing a bad "Bluray 2160p" as a good
	// "Bluray 2160p" produces the identical path: place() saw the file, said
	// "already imported", copied nothing — and then removeExistingFiles
	// skipped the old file too, because it is the one being kept. The row was
	// updated from sizeOf(dest), which re-read the OLD bytes, so the library
	// reported the new release at the old file's size and the user re-grabbed
	// the same movie over and over watching nothing change.
	if existing, err := os.Stat(dest); err == nil {
		if srcInfo, serr := os.Stat(src); serr == nil && os.SameFile(existing, srcInfo) {
			return applyMode(dest, fileMode()) // genuinely the same file
		}
		// Fall through and replace it. The write below goes to a temp file and
		// renames over the destination, so the old bytes survive right up
		// until the new ones are complete.
	}

	// Both paths land the file at a temp name first and rename over the
	// destination. Rename is atomic and, unlike Link, does not refuse when
	// something is already there — which is what makes replacement safe.
	tmp, err := placeTemp(ctx, dir, src, onBytes)
	if err != nil {
		return err
	}
	// Chmod the temp file BEFORE the rename, so the file is never visible at
	// its final path with the wrong mode.
	if err := applyMode(tmp, fileMode()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// placeTemp materialises src next to dest under a temporary name, hardlinking
// when the filesystem allows it and copying when it does not, and returns that
// name. The caller renames it into place.
func placeTemp(ctx context.Context, dir, src string, onBytes func(done, total int64)) (string, error) {
	// A hardlink is free and is what makes seeding-while-imported possible;
	// it only works within one filesystem, so a failure here is expected
	// rather than exceptional and falls through to the copy.
	//
	// CreateTemp reserves a name nothing else will pick, which matters because
	// two imports into one folder used to be able to choose the same one. The
	// file is removed immediately: Link needs the name free, and the window
	// between is smaller than the one a fixed name leaves open forever.
	if reserved, err := os.CreateTemp(dir, ".monarr-link-*"); err == nil {
		_, recoveryCopy := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext)
		link := reserved.Name()
		_ = reserved.Close()
		_ = os.Remove(link)
		if !recoveryCopy {
			if err := os.Link(src, link); err == nil {
				return link, nil
			}
		}
		_ = os.Remove(link)
	}

	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(dir, ".monarr-import-*")
	if err != nil {
		return "", err
	}
	tmp := out.Name()
	// The copy reports itself. This is the only place in Monarr where it
	// moves a large amount of data, and until it was instrumented a 20 GB
	// cross-filesystem copy was invisible: no bytes, no rate, no started-at,
	// nothing on any page. The one observable symptom was an unrelated
	// scheduled task showing a seventeen-minute duration.
	total := int64(0)
	if fi, serr := in.Stat(); serr == nil {
		total = fi.Size()
	}
	reader := &contextReader{ctx: ctx, r: &countingReader{r: in, total: total, on: onBytes}}
	if _, err := io.Copy(out, reader); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

// missingDirs returns the folders between dir and its nearest existing
// ancestor, outermost first — exactly the set MkdirAll is about to create,
// and therefore exactly the set monarr may set permissions on.
func missingDirs(dir string) []string {
	var missing []string
	for i := 0; i < 16; i++ {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		missing = append(missing, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Reverse: create-order, so a parent is chmodded before its child.
	for i, j := 0, len(missing)-1; i < j; i, j = i+1, j-1 {
		missing[i], missing[j] = missing[j], missing[i]
	}
	return missing
}

func sizeOf(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

// countingReader reports how far a copy has got, without changing what it
// copies. Wrapping the READER rather than the writer means the count is bytes
// genuinely read from the source, so a short write shows as a stall rather
// than as a copy that raced ahead of itself.
type countingReader struct {
	r     io.Reader
	done  int64
	total int64
	on    func(done, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.done += int64(n)
		if c.on != nil {
			// The callback throttles itself (transfers.Handle.Bytes); this
			// loop runs tens of thousands of times for a large file and must
			// not decide anything about how often anyone wants to hear.
			c.on(c.done, c.total)
		}
	}
	return n, err
}
