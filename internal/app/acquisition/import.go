package acquisition

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/filename"
	"github.com/monarr-media/monarr/internal/domain/naming"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
)

// importDownload moves a completed payload into the library: parse each
// video file, map it to wantables, apply per-file upgrade decisions,
// hardlink-or-copy into the Renamer layout, link MediaFile rows, and clean
// up replaced files (blueprint §5.1 grab → import).
func (s *Service) importDownload(ctx context.Context, dl sqlite.Download, savePath string) (int, bool, error) {
	item, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		return 0, false, err
	}

	// The import scope: which copy this grab was for decides the profile,
	// the destination folder, and which existing files count as "current".
	scope := importScope{Dest: item.Path, ProfileID: item.QualityProfileID}
	if dl.CopyID != 0 {
		cp, err := s.db.GetMediaCopy(ctx, dl.MediaItemID, dl.CopyID)
		if err != nil {
			return 0, false, fmt.Errorf("copy %d vanished: %w", dl.CopyID, err)
		}
		scope.CopyID = cp.ID
		scope.ProfileID = cp.QualityProfileID
		if cp.Path != "" {
			scope.Dest = cp.Path
		}
	}
	if scope.Dest == "" {
		return 0, false, fmt.Errorf("item has no library folder assigned")
	}

	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	videos, err := collectFiles(savePath, isMedia)
	if err != nil {
		return 0, false, err
	}
	if len(videos) == 0 {
		return 0, false, fmt.Errorf("no media files in %s", savePath)
	}

	epQuals, err := s.episodeQualities(ctx, item, scope.CopyID)
	if err != nil {
		return 0, false, err
	}
	profile, err := s.db.GetProfile(ctx, scope.ProfileID)
	if err != nil {
		return 0, false, err
	}

	imported, upgraded := 0, false
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

		var err error
		var wasUpgrade bool
		switch item.Kind {
		case domain.KindMovie:
			wasUpgrade, err = s.importMovieFile(ctx, item, scope, profile, src, q)
		case domain.KindBook:
			wasUpgrade, err = s.importBookFile(ctx, item, scope, src, q)
		default:
			wasUpgrade, err = s.importEpisodeFile(ctx, item, scope, profile, epQuals, src, p, q)
		}
		if err != nil {
			s.log.Warn("import: file skipped", "file", filepath.Base(src), "reason", err)
			continue
		}
		imported++
		upgraded = upgraded || wasUpgrade
	}
	if imported == 0 {
		return 0, false, fmt.Errorf("no files imported from %s", savePath)
	}

	_ = s.db.AddHistory(ctx, "imported", item.ID, dl.ReleaseTitle,
		map[string]any{"files": imported, "upgrade": upgraded})
	s.publish(ImportCompleted{MediaItemID: item.ID, Release: dl.ReleaseTitle,
		Files: imported, Upgrade: upgraded})
	s.log.Info("imported", "item", item.Title, "files", imported)
	return imported, upgraded, nil
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
// primary), its destination folder, and its quality profile.
type importScope struct {
	CopyID    int64
	Dest      string
	ProfileID int64
}

func (s *Service) importMovieFile(ctx context.Context, item domain.MediaItem, scope importScope, profile quality.Profile, src string, q quality.Quality) (bool, error) {
	current, have, err := s.db.BestQualityForItem(ctx, item.ID, scope.CopyID)
	if err != nil {
		return false, err
	}
	upgrade := false
	if have {
		if !quality.Better(q, current) {
			return false, fmt.Errorf("%s does not improve on %s", q.Display(), current.Display())
		}
		upgrade = true
	}
	_ = profile

	name := naming.Render(naming.MovieFileTemplate, map[string]string{
		"Movie Title": item.Title, "Release Year": strconv.Itoa(item.Year),
		"Quality Full": q.Display(),
	})
	dest := filepath.Join(scope.Dest, naming.SafeFileName(name)+filepath.Ext(src))
	if err := place(src, dest); err != nil {
		return false, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, nil, dest)
	}
	fileID, err := s.db.UpsertFile(ctx, item.ID, scope.CopyID, dest, sizeOf(dest))
	if err != nil {
		return false, err
	}
	return upgrade, s.db.SetFileQuality(ctx, fileID, q)
}

// importBookFile places one book file into <library>/<Author>/<Title>/ as
// "Title - Author.ext" (Calibre-friendly, ADR 0006), with the same
// upgrade-or-reject semantics as movies.
func (s *Service) importBookFile(ctx context.Context, item domain.MediaItem, scope importScope, src string, q quality.Quality) (bool, error) {
	current, have, err := s.db.BestQualityForItem(ctx, item.ID, scope.CopyID)
	if err != nil {
		return false, err
	}
	upgrade := false
	if have {
		if !quality.Better(q, current) {
			return false, fmt.Errorf("%s does not improve on %s", q.Display(), current.Display())
		}
		upgrade = true
	}

	dest := filepath.Join(scope.Dest,
		naming.BookFileName(item.Author, item.Title)+strings.ToLower(filepath.Ext(src)))
	if err := place(src, dest); err != nil {
		return false, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, nil, dest)
	}
	fileID, err := s.db.UpsertFile(ctx, item.ID, scope.CopyID, dest, sizeOf(dest))
	if err != nil {
		return false, err
	}
	return upgrade, s.db.SetFileQuality(ctx, fileID, q)
}

func (s *Service) importEpisodeFile(ctx context.Context, item domain.MediaItem, scope importScope, profile quality.Profile, epQuals map[int64]*quality.Quality, src string, p parser.Parsed, q quality.Quality) (bool, error) {
	season, eps := p.Season, p.Episodes
	if len(eps) == 0 {
		if fx, ok := filename.Extract(filepath.Base(src)); ok {
			season, eps = fx.Season, fx.Episodes
		}
	}
	if len(eps) == 0 || season < 0 {
		return false, fmt.Errorf("cannot determine episodes from %q", filepath.Base(src))
	}

	// Resolve episode ids + titles; per-file upgrade check against the
	// WORST current quality among covered episodes.
	var epIDs []int64
	var epTitles []string
	var worst *quality.Quality
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
		if cur := epQuals[epID]; cur == nil {
			missing = true
		} else if worst == nil || quality.Better(*worst, *cur) {
			worst = cur
		}
	}
	if len(epIDs) == 0 {
		return false, fmt.Errorf("no known episodes for S%02d %v", season, eps)
	}
	upgrade := false
	if !missing && worst != nil {
		if !quality.Better(q, *worst) {
			return false, fmt.Errorf("%s does not improve on %s", q.Display(), worst.Display())
		}
		upgrade = true
	}
	_ = profile

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
		return false, err
	}
	if upgrade {
		s.removeExistingFiles(ctx, item, scope.CopyID, epIDs, dest)
	}
	fileID, err := s.db.UpsertFile(ctx, item.ID, scope.CopyID, dest, sizeOf(dest))
	if err != nil {
		return false, err
	}
	if err := s.db.SetFileQuality(ctx, fileID, q); err != nil {
		return false, err
	}
	return upgrade, s.db.ReplaceFileEpisodeLinks(ctx, fileID, epIDs)
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

// place hardlinks src to dest, falling back to copy across filesystems.
func place(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		return nil // already imported (idempotent re-poll)
	}
	if err := os.Link(src, dest); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(filepath.Dir(dest), ".monarr-import-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func sizeOf(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}
