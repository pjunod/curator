package acquisition

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/filename"
	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/naming"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
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
	// Path is where the file landed — absolute, as Monarr sees it. Empty
	// when the file was skipped.
	Path string `json:"path,omitempty"`
}

// ImportResult is the outcome of one payload.
type ImportResult struct {
	Imported int
	Upgraded bool
	Files    []FileOutcome
}

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
	return ImportCompleted{
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
	item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		return ImportResult{}, err
	}

	// The import scope: which copy this grab was for decides the profile,
	// the destination folder, and which existing files count as "current".
	scope := importScope{Dest: item.Path, ProfileID: item.QualityProfileID,
		Release: dl.ReleaseTitle, Indexer: dl.Indexer}
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
		return ImportResult{}, fmt.Errorf("item has no library folder assigned")
	}

	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	videos, err := collectFiles(savePath, isMedia)
	if err != nil {
		return ImportResult{}, err
	}
	if len(videos) == 0 {
		return ImportResult{}, fmt.Errorf("no media files in %s", savePath)
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

	result := ImportResult{Files: make([]FileOutcome, 0, len(videos))}
	for _, src := range videos {
		p := parser.Parse(filepath.Base(src))
		q := p.Quality
		if item.Kind == domain.KindBook {
			// The extension is the authority on a book file's format.
			if ext := filename.BookQualitySource(src); ext != "" {
				q = quality.Quality{Source: quality.Source(ext)}
			}
		} else if q.Source == quality.SourceUnknown && q.Resolution == 0 {
			// Single files often carry the quality only on the release name.
			q = dl.Quality
		}

		outcome := FileOutcome{Name: filepath.Base(src), Quality: q.Display()}
		var err error
		var put placement
		switch item.Kind {
		case domain.KindMovie:
			put, err = s.importMovieFile(ctx, item, scope, profile, src, q, dl.ReleaseTitle, manual)
		case domain.KindBook:
			put, err = s.importBookFile(ctx, item, scope, profile, src, q, manual)
		default:
			put, err = s.importEpisodeFile(ctx, item, scope, profile, epStates, src, p, q, dl.ReleaseTitle, manual)
		}
		if err != nil {
			outcome.Reason = err.Error()
			s.log.Warn("import: file skipped", "file", outcome.Name, "reason", err)
		} else {
			outcome.Imported, outcome.Upgrade = true, put.Upgrade
			outcome.Path = put.Path
			result.Imported++
			result.Upgraded = result.Upgraded || put.Upgrade
		}
		result.Files = append(result.Files, outcome)
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
	CopyID    int64
	Dest      string
	ProfileID int64
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

	name := naming.Render(naming.MovieFileTemplate, map[string]string{
		"Movie Title": item.Title, "Release Year": strconv.Itoa(item.Year),
		"Quality Full": q.Display(),
	})
	dest := filepath.Join(scope.Dest, naming.SafeFileName(name)+filepath.Ext(src))
	if err := place(src, dest); err != nil {
		return placement{}, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, nil, dest)
	}
	fileID, err := s.db.UpsertFile(ctx, item.ID, scope.CopyID, dest, sizeOf(dest))
	if err != nil {
		return placement{}, err
	}
	s.rememberSource(ctx, fileID, dest, scope)
	// The file exists now, so stop taking the release name's word for it.
	s.recordImportedQuality(ctx, item.ID, fileID, dest, q, releaseTitle, item.Runtime)
	return placement{Path: dest, Upgrade: upgrade}, nil
}

// importBookFile places one book file into <library>/<Author>/<Title>/ as
// "Title - Author.ext" (Calibre-friendly, ADR 0006), with the same
// upgrade-or-reject semantics as movies.
func (s *Service) importBookFile(ctx context.Context, item domain.MediaItem, scope importScope, profile quality.Profile, src string, q quality.Quality, manual bool) (placement, error) {
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
	if err := place(src, dest); err != nil {
		return placement{}, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, nil, dest)
	}
	fileID, err := s.db.UpsertFile(ctx, item.ID, scope.CopyID, dest, sizeOf(dest))
	if err != nil {
		return placement{}, err
	}
	s.rememberSource(ctx, fileID, dest, scope)
	// Books are not probed: the extension IS the format (ADR 0006), so the
	// provenance is the filename and there is nothing to measure.
	return placement{Path: dest, Upgrade: upgrade},
		s.db.SetFileQualityFrom(ctx, fileID, q,
			mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone)
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

	epToken := fmt.Sprintf("S%02dE%02d", season, eps[0])
	if len(eps) > 1 {
		epToken += fmt.Sprintf("-E%02d", eps[len(eps)-1])
	}
	title := ""
	if len(epTitles) > 0 {
		title = epTitles[0]
	}
	base := fmt.Sprintf("%s - %s - %s [%s]", item.Title, epToken, title, q.Display())
	dest := filepath.Join(scope.Dest,
		naming.Render(naming.SeasonFolderTemplate, map[string]string{"season": strconv.Itoa(season)}),
		naming.SafeFileName(base)+filepath.Ext(src))
	if err := place(src, dest); err != nil {
		return placement{}, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, epIDs, dest)
	}
	fileID, err := s.db.UpsertFile(ctx, item.ID, scope.CopyID, dest, sizeOf(dest))
	if err != nil {
		return placement{}, err
	}
	s.rememberSource(ctx, fileID, dest, scope)
	s.recordImportedQuality(ctx, item.ID, fileID, dest, q, releaseTitle, item.Runtime)
	return placement{Path: dest, Upgrade: upgrade},
		s.db.ReplaceFileEpisodeLinks(ctx, fileID, epIDs)
}

// rememberSource records which release put a file on disk.
//
// Not bookkeeping. It is the only way somebody staring at a bad file three
// weeks later can say "and never take that release again" without going to
// find the download in the queue, which by then is long gone. A failure here
// is worth a line in the log and nothing more — the file imported fine, and
// refusing the import over a missing audit field would be the tail wagging
// the dog.
func (s *Service) rememberSource(ctx context.Context, fileID int64, dest string, scope importScope) {
	if scope.Release == "" {
		return // a manual import of a folder nobody grabbed
	}
	if err := s.db.SetFileSource(ctx, fileID, scope.Release, scope.Indexer); err != nil {
		s.log.Warn("import: could not record source release",
			"file", filepath.Base(dest), "err", err)
	}
}

// removeExistingFiles deletes replaced files (rows + disk) for the target
// scope: all of ONE COPY's item files for movies, or that copy's files
// linked to the given episodes. Other copies' files are never touched —
// upgrading the 4K primary must not delete the 720p copy.
func (s *Service) removeExistingFiles(ctx context.Context, item domain.MediaItem, copyID int64, episodeIDs []int64, keep string) {
	files, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return
	}
	covered := map[int64]bool{}
	for _, id := range episodeIDs {
		covered[id] = true
	}
	for _, f := range files {
		if f.Path == keep || f.CopyID != copyID {
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
		if err := s.db.DeleteFile(ctx, f.ID); err == nil {
			if rmErr := os.Remove(f.Path); rmErr != nil && !os.IsNotExist(rmErr) {
				s.log.Warn("import: could not remove replaced file", "path", f.Path, "err", rmErr)
			}
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
func place(src, dest string) error {
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
	tmp, err := placeTemp(dir, src)
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
func placeTemp(dir, src string) (string, error) {
	// A hardlink is free and is what makes seeding-while-imported possible;
	// it only works within one filesystem, so a failure here is expected
	// rather than exceptional and falls through to the copy.
	//
	// CreateTemp reserves a name nothing else will pick, which matters because
	// two imports into one folder used to be able to choose the same one. The
	// file is removed immediately: Link needs the name free, and the window
	// between is smaller than the one a fixed name leaves open forever.
	if reserved, err := os.CreateTemp(dir, ".monarr-link-*"); err == nil {
		link := reserved.Name()
		_ = reserved.Close()
		_ = os.Remove(link)
		if err := os.Link(src, link); err == nil {
			return link, nil
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
	if _, err := io.Copy(out, in); err != nil {
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
