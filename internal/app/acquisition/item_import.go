package acquisition

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/naming"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Import file… on the item page (v0.45.0). The target item is known, so a
// person browses to a folder, picks files, confirms one plan, and the files
// go through the paths that already exist: a file inside a runner delivery
// through Resolve's placement, anything else through the queued manual
// import. This file holds the parts the item page adds: the one-level folder
// listing, the confirm-step plan, the chosen episodes, subtitles of manual
// imports, and Remove original.

// ScannedPublication names the runner delivery a scanned file belongs to.
type ScannedPublication struct {
	DownloadID    int64  `json:"downloadId"`
	PublicationID int64  `json:"publicationId"`
	Path          string `json:"path"`    // relative to the publication, slash-separated
	Outcome       string `json:"outcome"` // the file's recorded outcome ("" when the manifest does not name it)
}

type manualEpisodeTargetsKey struct{}

func withManualEpisodeTargets(ctx context.Context, targets map[string]sqlite.HandoffEpisodeTarget) context.Context {
	if len(targets) == 0 {
		return ctx
	}
	clean := make(map[string]sqlite.HandoffEpisodeTarget, len(targets))
	for p, t := range targets {
		clean[filepath.Clean(p)] = t
	}
	return context.WithValue(ctx, manualEpisodeTargetsKey{}, clean)
}

func manualEpisodeTargets(ctx context.Context) map[string]sqlite.HandoffEpisodeTarget {
	targets, _ := ctx.Value(manualEpisodeTargetsKey{}).(map[string]sqlite.HandoffEpisodeTarget)
	return targets
}

func episodeToken(season int, eps []int) string {
	if len(eps) == 0 {
		return fmt.Sprintf("S%02d", season)
	}
	token := fmt.Sprintf("S%02dE%02d", season, eps[0])
	if len(eps) > 1 {
		token += fmt.Sprintf("-E%02d", eps[len(eps)-1])
	}
	return token
}

// knownEpisodes refuses an episode choice the series does not have.
func (s *Service) knownEpisodes(ctx context.Context, itemID int64, season int, eps []int) error {
	if season < 0 || len(eps) == 0 {
		return fmt.Errorf("choose the season and episode this file is")
	}
	for _, ep := range eps {
		if _, err := s.db.GetEpisodeID(ctx, itemID, season, ep); err != nil {
			if errors.Is(err, sqlite.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%s is not an episode of this series", episodeToken(season, []int{ep}))
			}
			return err
		}
	}
	return nil
}

// admitEpisodeTargets validates a manual import's episode choices against
// the admitted selection: each names one selected file and real episodes.
func (s *Service) admitEpisodeTargets(ctx context.Context, item domain.MediaItem, admitted []string, targets map[string]sqlite.HandoffEpisodeTarget) (map[string]sqlite.HandoffEpisodeTarget, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	if item.Kind != domain.KindSeries {
		return nil, fmt.Errorf("episodes apply only to a series")
	}
	selected := make(map[string]bool, len(admitted))
	for _, p := range admitted {
		selected[filepath.Clean(p)] = true
	}
	out := make(map[string]sqlite.HandoffEpisodeTarget, len(targets))
	for p, t := range targets {
		p = filepath.Clean(p)
		if !selected[p] {
			return nil, fmt.Errorf("an episode was chosen for %s, which is not a selected file", p)
		}
		if err := s.knownEpisodes(ctx, item.ID, t.Season, t.Episodes); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		out[p] = sqlite.HandoffEpisodeTarget{Season: t.Season, Episodes: append([]int(nil), t.Episodes...)}
	}
	return out, nil
}

// unreadableImportPath is the plain answer to "Curator cannot see that
// folder": it names the path and the fix.
func unreadableImportPath(dir string, err error) error {
	return fmt.Errorf("path not accessible: Curator cannot read %s (%v). If the download client sees this folder under a different path, add a remote path mapping to that client in Settings → Acquisition so Curator reads it where it is mounted here", dir, err)
}

// ScanImportFolder lists the media files directly in dir (one level, no
// descent), each with the subtitles that would travel with it and, for a
// file inside a runner delivery, that delivery.
func (s *Service) ScanImportFolder(ctx context.Context, dir string) ([]ScannedFile, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, fmt.Errorf("no path given")
	}
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, unreadableImportPath(dir, err)
	}
	if !info.IsDir() {
		return s.ScanImportPath(ctx, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, unreadableImportPath(dir, err)
	}
	out := []ScannedFile{}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || (!filename.IsVideo(p) && !filename.IsBook(p)) || isReleaseSample(p) {
			continue
		}
		files, err := s.ScanImportPath(ctx, p)
		if err != nil {
			continue
		}
		out = append(out, files...)
	}
	counts := subtitleCounts(dir)
	for i := range out {
		out[i].Subtitles = counts[out[i].Name]
		pub, err := s.scannedPublication(ctx, out[i].Path)
		if err != nil {
			return nil, err
		}
		out[i].Publication = pub
	}
	return out, nil
}

// scannedPublication finds the runner delivery a file is part of, if any.
func (s *Service) scannedPublication(ctx context.Context, abs string) (*ScannedPublication, error) {
	pub, ok, err := s.publicationClaiming(ctx, abs)
	if err != nil || !ok {
		return nil, err
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(storagePath(filepath.Clean(pub.Path), roots), storagePath(filepath.Clean(abs), roots))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = filepath.Base(abs)
	}
	out := &ScannedPublication{DownloadID: pub.DownloadID, PublicationID: pub.ID, Path: filepath.ToSlash(rel)}
	rows, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Path == out.Path {
			out.Outcome = r.Outcome
		}
	}
	return out, nil
}

// subtitleFolder reports a folder named for subtitles ("Subs", "Subtitles").
func subtitleFolder(name string) bool {
	switch strings.ToLower(name) {
	case "subs", "sub", "subtitles", "subtitle":
		return true
	}
	return false
}

// folderSubtitles lists the subtitles that belong to the videos directly in
// dir, relative to it: those beside them, in a Subs folder, or in a folder
// of a Subs folder (Subs/<video>/2_English.srt). Other folders are other
// releases and are not looked at.
func folderSubtitles(dir string) []string {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			if isSubtitleFile(e.Name()) {
				out = append(out, e.Name())
			}
			continue
		}
		if !subtitleFolder(e.Name()) {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range inner {
			rel := e.Name() + "/" + f.Name()
			if !f.IsDir() {
				if isSubtitleFile(f.Name()) {
					out = append(out, rel)
				}
				continue
			}
			deep, err := os.ReadDir(filepath.Join(dir, e.Name(), f.Name()))
			if err != nil {
				continue
			}
			for _, d := range deep {
				if !d.IsDir() && isSubtitleFile(d.Name()) {
					out = append(out, rel+"/"+d.Name())
				}
			}
		}
	}
	return out
}

// folderVideos lists the videos directly in dir (not samples), by name.
func folderVideos(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filename.IsVideo(e.Name()) && !isReleaseSample(filepath.Join(dir, e.Name())) {
			out = append(out, e.Name())
		}
	}
	return out
}

// subtitleCounts says how many subtitle files go with each video in dir,
// by the association a runner delivery's subtitles use.
func subtitleCounts(dir string) map[string]int {
	counts := map[string]int{}
	subs := folderSubtitles(dir)
	if len(subs) == 0 {
		return counts
	}
	var videos []publicationVideo
	for _, v := range folderVideos(dir) {
		videos = append(videos, publicationVideo{rel: v, library: v})
	}
	files := make([]ports.ManifestFile, 0, len(subs))
	for _, sub := range subs {
		files = append(files, ports.ManifestFile{Path: sub})
	}
	for _, unit := range subtitleUnits(files) {
		if v, ok := subtitleVideo(unit[0].Path, videos); ok {
			counts[v.rel] += len(unit)
		}
	}
	return counts
}

// placeManualSubtitles places the subtitles of each video a standalone
// manual import placed, beside it and named after it, using the runner
// delivery's association and naming. A subtitle that cannot be placed is
// logged and left; it never fails the import. It returns the sources placed.
func (s *Service) placeManualSubtitles(ctx context.Context, result ImportResult) []string {
	byDir := map[string]map[string]string{} // source folder → video name → library path
	for _, f := range result.Files {
		if !f.Imported || f.Path == "" || f.SourcePath == "" {
			continue
		}
		dir := filepath.Dir(f.SourcePath)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		byDir[dir][filepath.Base(f.SourcePath)] = f.Path
	}
	var placed []string
	for dir, libraryOf := range byDir {
		subs := folderSubtitles(dir)
		if len(subs) == 0 {
			continue
		}
		var videos []publicationVideo
		for _, v := range folderVideos(dir) {
			videos = append(videos, publicationVideo{rel: v, library: libraryOf[v]})
		}
		files := make([]ports.ManifestFile, 0, len(subs))
		for _, sub := range subs {
			files = append(files, ports.ManifestFile{Path: sub})
		}
		used := map[string]bool{}
		for _, unit := range subtitleUnits(files) {
			video, ok := subtitleVideo(unit[0].Path, videos)
			if !ok || video.library == "" {
				continue
			}
			base := subtitleBaseName(unit[0].Path, video)
			libDir := filepath.Dir(video.library)
			name := ""
			for n := 1; n < 100 && name == ""; n++ {
				candidate := base
				if n > 1 {
					candidate = base + "." + strconv.Itoa(n)
				}
				if used[strings.ToLower(filepath.Join(libDir, candidate))] {
					continue
				}
				if manualSubtitleNameFree(ctx, dir, libDir, candidate, unit) {
					name = candidate
				}
			}
			if name == "" {
				s.log.Info("import: subtitle not placed; no free name beside its video", "file", unit[0].Path)
				continue
			}
			used[strings.ToLower(filepath.Join(libDir, name))] = true
			for _, f := range unit {
				src := filepath.Join(dir, filepath.FromSlash(f.Path))
				dest := filepath.Join(libDir, name+strings.ToLower(path.Ext(f.Path)))
				if same, _ := sameFileBytes(ctx, src, dest); !same {
					if err := placeSubtitle(ctx, src, dest, ""); err != nil {
						s.log.Info("import: subtitle not placed", "file", src, "why", err)
						continue
					}
				}
				placed = append(placed, src)
			}
		}
	}
	return placed
}

// manualSubtitleNameFree: nothing is at the unit's names, or what is there
// is already these bytes (a retry). A file someone else put there is never
// overwritten.
func manualSubtitleNameFree(ctx context.Context, srcDir, libDir, name string, unit []ports.ManifestFile) bool {
	for _, f := range unit {
		dest := filepath.Join(libDir, name+strings.ToLower(path.Ext(f.Path)))
		if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if same, _ := sameFileBytes(ctx, filepath.Join(srcDir, filepath.FromSlash(f.Path)), dest); !same {
			return false
		}
	}
	return true
}

func sameFileBytes(ctx context.Context, a, b string) (bool, error) {
	da, _, err := fileDigest(ctx, a)
	if err != nil {
		return false, err
	}
	db, _, err := fileDigest(ctx, b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

// removeManualOriginals removes the sources of a standalone manual import
// that asked for it, once the import completed: each video only when its
// library copy is in place with the same size (the placement proved the
// bytes as it wrote them, or the library file is a link to the same object),
// and the subtitles that travelled with them. Same filesystem, this makes
// the import a rename; across volumes, a copy, verify and unlink.
func (s *Service) removeManualOriginals(ctx context.Context, dl sqlite.Download, result ImportResult) {
	var removed, kept []string
	remove := func(src string) {
		if err := os.Remove(src); err != nil && !errors.Is(err, os.ErrNotExist) {
			kept = append(kept, src+": "+err.Error())
			return
		}
		removed = append(removed, src)
	}
	for _, f := range result.Files {
		if !f.Imported || f.Path == "" || f.SourcePath == "" || filepath.Clean(f.Path) == filepath.Clean(f.SourcePath) {
			continue
		}
		src, srcErr := os.Lstat(f.SourcePath)
		lib, libErr := os.Lstat(f.Path)
		if srcErr != nil || libErr != nil || !src.Mode().IsRegular() || !lib.Mode().IsRegular() || src.Size() != lib.Size() {
			kept = append(kept, f.SourcePath+": its library copy could not be confirmed")
			continue
		}
		remove(f.SourcePath)
	}
	for _, sub := range result.Sidecars {
		remove(sub)
	}
	if len(removed) > 0 || len(kept) > 0 {
		_ = s.db.AddHistory(ctx, HistoryManualOriginalsRemoved, dl.MediaItemID, dl.ReleaseTitle,
			map[string]any{"removed": removed, "kept": kept})
		s.log.Info("manual import: originals removed", "download", dl.ID, "removed", len(removed), "kept", len(kept))
	}
}

// HistoryManualOriginalsRemoved records what Remove original removed.
const HistoryManualOriginalsRemoved = "manual_import_originals_removed"

// ItemImportFile is one selected file and, for a series, the episode(s) a
// person chose (empty: read from the name).
type ItemImportFile struct {
	Path     string
	Season   int
	Episodes []int
}

// ItemImportPlanRequest asks what importing these files into an item would do.
type ItemImportPlanRequest struct {
	MediaItemID    int64
	CopyID         int64
	RemoveOriginal bool
	Files          []ItemImportFile
}

// ItemImportPlanFile is the confirm step's line for one file.
type ItemImportPlanFile struct {
	Path        string              `json:"path"`
	Quality     string              `json:"quality"`
	Season      int                 `json:"season"`
	Episodes    []int               `json:"episodes"`
	Destination string              `json:"destination,omitempty"`
	Action      string              `json:"action,omitempty"` // new | replace | beside
	Existing    string              `json:"existing,omitempty"`
	Reason      string              `json:"reason,omitempty"`
	Mode        string              `json:"mode,omitempty"` // runner | hardlink | copy | rename | copy_remove
	Subtitles   int                 `json:"subtitles"`
	Publication *ScannedPublication `json:"publication,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// PlanItemImport answers the confirm step without moving anything: per
// file, the exact destination from the naming template, what happens to a
// file already there (replaced when the new one ranks higher in the profile,
// otherwise kept beside it), and how the bytes get there. It mirrors the
// manual import's own rules; the import decides again when it runs.
func (s *Service) PlanItemImport(ctx context.Context, req ItemImportPlanRequest) ([]ItemImportPlanFile, error) {
	if req.MediaItemID == 0 || len(req.Files) == 0 {
		return nil, fmt.Errorf("an item and at least one file are required")
	}
	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return nil, err
	}
	dest, profileID := item.Path, item.QualityProfileID
	if req.CopyID != 0 {
		cp, err := s.db.GetMediaCopy(ctx, item.ID, req.CopyID)
		if err != nil {
			return nil, fmt.Errorf("copy %d is not a copy of this item", req.CopyID)
		}
		profileID = cp.QualityProfileID
		if cp.Path != "" {
			dest = cp.Path
		}
	}
	if dest == "" {
		return nil, ErrNoLibraryFolder
	}
	profile, err := s.db.GetProfile(ctx, profileID)
	if err != nil {
		return nil, err
	}
	var epStates map[int64]episodeState
	if item.Kind == domain.KindSeries {
		if epStates, err = s.episodeStates(ctx, item, req.CopyID); err != nil {
			return nil, err
		}
	}
	disk, err := s.db.DiskStateForItem(ctx, item.ID, req.CopyID)
	if err != nil {
		return nil, err
	}
	out := make([]ItemImportPlanFile, 0, len(req.Files))
	for _, f := range req.Files {
		line := s.planItemFile(ctx, item, profile, dest, disk, epStates, req.RemoveOriginal, f)
		out = append(out, line)
	}
	return out, nil
}

func (s *Service) planItemFile(ctx context.Context, item domain.MediaItem, profile quality.Profile, root string, disk sqlite.CopyDiskState, epStates map[int64]episodeState, removeOriginal bool, f ItemImportFile) ItemImportPlanFile {
	src := filepath.Clean(f.Path)
	base := filepath.Base(src)
	line := ItemImportPlanFile{Path: src, Season: -1, Episodes: []int{}}
	info, err := os.Lstat(src)
	if err != nil || !info.Mode().IsRegular() {
		line.Error = "not a readable file"
		if err != nil {
			line.Error = unreadableImportPath(src, err).Error()
		}
		return line
	}
	if !deliveryImportablePath(item.Kind, src) {
		line.Error = "not media of this item's kind"
		return line
	}
	line.Subtitles = subtitleCounts(filepath.Dir(src))[base]
	pub, err := s.scannedPublication(ctx, src)
	if err != nil {
		line.Error = err.Error()
		return line
	}
	line.Publication = pub
	if pub != nil {
		switch {
		case pub.DownloadID == 0:
			line.Error = "this runner delivery has no Activity row to place it from"
			return line
		case pub.Outcome != "" && pub.Outcome != "unresolved" && pub.Outcome != "pending":
			line.Error = "runner's delivery already recorded this file as " + pub.Outcome + "; it cannot be placed again"
			return line
		}
	}

	var q quality.Quality
	if item.Kind == domain.KindBook {
		q = quality.Quality{Source: quality.Source(filename.BookQualitySource(src))}
	} else {
		q = parser.Parse(base).Quality
	}
	line.Quality = q.Display()

	var have *quality.Quality
	verified, audio := disk.SourceVerified, disk.Audio
	langs := importLanguages(base, src)
	switch item.Kind {
	case domain.KindSeries:
		season, eps := f.Season, f.Episodes
		if len(eps) == 0 {
			if fx, ok := filename.Extract(base); ok {
				season, eps = fx.Season, fx.Episodes
			} else if p := parser.Parse(base); len(p.Episodes) > 0 {
				season, eps = p.Season, p.Episodes
			}
		}
		if len(eps) == 0 {
			line.Error = "choose the episode this file is; its name does not say"
			return line
		}
		line.Season, line.Episodes = season, append([]int(nil), eps...)
		title := ""
		for _, ep := range eps {
			id, err := s.db.GetEpisodeID(ctx, item.ID, season, ep)
			if err != nil {
				line.Error = episodeToken(season, []int{ep}) + " is not an episode of this series"
				return line
			}
			st := epStates[id]
			if st.Have != nil && (have == nil || quality.Better(*have, *st.Have)) {
				have, verified, audio = st.Have, st.Verified, st.Audio
			}
			if title == "" {
				title = episodeTitle(item, season, ep)
			}
		}
		line.Destination = episodeDestination(root, item, src, q, season, eps, title)
	case domain.KindBook:
		have = disk.Best
		langs = nil
		line.Destination = filepath.Join(root, naming.BookFileName(item.Author, item.Title)+strings.ToLower(filepath.Ext(src)))
	default:
		have = disk.Best
		line.Destination = movieDestination(root, item, src, q)
	}

	switch {
	case have == nil:
		line.Action, line.Reason = "new", "nothing is there yet"
	case profile.Upgrade(q, langs, *have, verified, audio) || (quality.Better(q, *have) && (item.Kind == domain.KindBook || profile.LanguageAcceptable(langs))):
		line.Action, line.Existing = "replace", have.Display()
		line.Reason = fmt.Sprintf("%s ranks above the %s here in the %s profile; the old file is removed once this one is placed", q.Display(), have.Display(), profile.Name)
	default:
		line.Action, line.Existing = "beside", have.Display()
		line.Reason = fmt.Sprintf("%s does not rank above the %s here in the %s profile; it is kept beside it", q.Display(), have.Display(), profile.Name)
		if _, err := os.Lstat(line.Destination); err == nil {
			line.Reason += " under a [manual-…] name, since a file already has this one"
		}
	}

	switch {
	case pub != nil:
		line.Mode = "runner"
	case sameStorage(src, line.Destination):
		line.Mode = "hardlink"
		if removeOriginal {
			line.Mode = "rename"
		}
	default:
		line.Mode = "copy"
		if removeOriginal {
			line.Mode = "copy_remove"
		}
	}
	return line
}

func episodeTitle(item domain.MediaItem, season, ep int) string {
	for _, se := range item.Seasons {
		if se.Number != season {
			continue
		}
		for _, e := range se.Episodes {
			if e.EpisodeNumber == ep {
				return e.Title
			}
		}
	}
	return ""
}

// sameStorage reports that src and dest (or dest's nearest existing folder)
// are on one filesystem, so a placement can be a link instead of a copy.
func sameStorage(src, dest string) bool {
	a, err := os.Stat(src)
	if err != nil {
		return false
	}
	for dir := filepath.Dir(dest); ; dir = filepath.Dir(dir) {
		if b, err := os.Stat(dir); err == nil {
			da, okA := fileDevice(a)
			db, okB := fileDevice(b)
			return okA && okB && da == db
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false
		}
	}
}
