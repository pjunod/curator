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

// ErrInvalidImportPlan marks a plan request that cannot be answered as asked
// (no item, no files, a copy that is not the item's).
var ErrInvalidImportPlan = errors.New("invalid import plan")

// nameEpisodes is the season and episode(s) a file's name says, read the way
// the import reads it (the parser first, then the file-name extractor).
func nameEpisodes(p parser.Parsed, base string) (int, []int) {
	if len(p.Episodes) > 0 {
		return p.Season, p.Episodes
	}
	if fx, ok := filename.Extract(base); ok {
		return fx.Season, fx.Episodes
	}
	return p.Season, nil
}

// publicationLookup answers "which runner delivery is this file part of"
// for many files of one request: the completed roots, the publications and
// each publication's file rows are read once.
type publicationLookup struct {
	s     *Service
	roots []string
	pubs  []sqlite.Publication
	dirs  []string
	files map[int64]map[string]sqlite.PublicationFile
}

func (s *Service) newPublicationLookup(ctx context.Context) (*publicationLookup, error) {
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return nil, err
	}
	pubs, err := s.db.ListPublicationPaths(ctx)
	if err != nil {
		return nil, err
	}
	l := &publicationLookup{s: s, roots: roots, pubs: pubs, files: map[int64]map[string]sqlite.PublicationFile{}}
	for _, p := range pubs {
		l.dirs = append(l.dirs, storagePath(filepath.Clean(p.Path), roots))
	}
	return l, nil
}

// find returns the publication a file is inside, and its file row when the
// manifest names it (Outcome "" when it does not).
func (l *publicationLookup) find(ctx context.Context, abs string) (*ScannedPublication, error) {
	target := storagePath(filepath.Clean(abs), l.roots)
	for i, p := range l.pubs {
		if !within(l.dirs[i], target) {
			continue
		}
		rel, err := filepath.Rel(l.dirs[i], target)
		if err != nil {
			rel = filepath.Base(abs)
		}
		out := &ScannedPublication{DownloadID: p.DownloadID, PublicationID: p.ID, Path: filepath.ToSlash(rel)}
		rows, ok := l.files[p.ID]
		if !ok {
			list, err := l.s.db.ListPublicationFiles(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			rows = make(map[string]sqlite.PublicationFile, len(list))
			for _, r := range list {
				rows[r.Path] = r
			}
			l.files[p.ID] = rows
		}
		out.Outcome = rows[out.Path].Outcome
		return out, nil
	}
	return nil, nil
}

// ScanImportFolder lists the media files directly in dir (one level, no
// descent), each with the subtitles that would travel with it and, for a
// file inside a runner delivery, that delivery. An entry it cannot offer (a
// symlink, an unreadable file) is listed with the reason instead of being
// left out.
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
	lookup, err := s.newPublicationLookup(ctx)
	if err != nil {
		return nil, err
	}
	counts := subtitleCounts(dir)
	out := []ScannedFile{}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || (!filename.IsVideo(p) && !filename.IsBook(p)) || isReleaseSample(p) {
			continue
		}
		skipped := ScannedFile{Path: p, Name: e.Name(), Season: -1, Episodes: []int{}, Kind: "video"}
		if filename.IsBook(p) {
			skipped.Kind = "book"
		}
		if e.Type()&os.ModeSymlink != 0 {
			skipped.Skipped = "a symlink; Curator imports only regular files (import the file it points to)"
			out = append(out, skipped)
			continue
		}
		files, err := s.ScanImportPath(ctx, p)
		if err != nil || len(files) == 0 {
			skipped.Skipped = "not a readable regular file"
			if err != nil {
				skipped.Skipped = err.Error()
			}
			out = append(out, skipped)
			continue
		}
		for _, f := range files {
			f.Subtitles = counts[f.Name]
			if f.Publication, err = lookup.find(ctx, f.Path); err != nil {
				return nil, err
			}
			out = append(out, f)
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

// folderSubtitles lists the subtitles that may belong to the videos directly
// in dir, relative to it: those beside them, in a Subs folder, or in a
// folder of a Subs folder (Subs/<video>/2_English.srt). Other folders are
// other releases and are not looked at.
func folderSubtitles(dir string) []string {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			if e.Type().IsRegular() && isSubtitleFile(e.Name()) {
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
				if f.Type().IsRegular() && isSubtitleFile(f.Name()) {
					out = append(out, rel)
				}
				continue
			}
			deep, err := os.ReadDir(filepath.Join(dir, e.Name(), f.Name()))
			if err != nil {
				continue
			}
			for _, d := range deep {
				if d.Type().IsRegular() && isSubtitleFile(d.Name()) {
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
		if e.Type().IsRegular() && filename.IsVideo(e.Name()) && !isReleaseSample(filepath.Join(dir, e.Name())) {
			out = append(out, e.Name())
		}
	}
	return out
}

// looseSubtitleVideo is the video a subtitle in a loose folder belongs to.
// A loose folder (a downloads folder) holds unrelated files side by side, so
// a subtitle beside the videos belongs only to the video its name starts
// with; one in a Subs folder is matched as in a runner delivery.
func looseSubtitleVideo(rel string, videos []publicationVideo) (publicationVideo, bool) {
	if path.Dir(rel) != "." {
		return subtitleVideo(rel, videos)
	}
	stem := strings.ToLower(pathStem(rel))
	best, bestLen, ties := publicationVideo{}, 0, 0
	for _, v := range videos {
		vstem := strings.ToLower(pathStem(v.rel))
		if !strings.HasPrefix(stem, vstem) {
			continue
		}
		switch {
		case len(vstem) > bestLen:
			best, bestLen, ties = v, len(vstem), 1
		case len(vstem) == bestLen:
			ties++
		}
	}
	return best, bestLen > 0 && ties == 1
}

// subtitleCounts says how many subtitle files go with each video in dir.
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
		if v, ok := looseSubtitleVideo(unit[0].Path, videos); ok {
			counts[v.rel] += len(unit)
		}
	}
	return counts
}

// placeManualSubtitles places the subtitles of each video a standalone
// manual import placed, beside it and named after it, using the runner
// delivery's naming. A subtitle that cannot be placed is logged and left; it
// never fails the import. A cancelled import stops here too. It returns
// each placed subtitle's source and library path, with the source's identity
// as it was copied.
func (s *Service) placeManualSubtitles(ctx context.Context, result ImportResult) []sqlite.HandoffRemoval {
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
	var placed []sqlite.HandoffRemoval
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
			if ctx.Err() != nil {
				return placed
			}
			video, ok := looseSubtitleVideo(unit[0].Path, videos)
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
				info, err := os.Lstat(src)
				if err != nil {
					continue
				}
				if same, _ := sameFileBytes(ctx, src, dest); !same {
					if err := placeSubtitle(ctx, src, dest, ""); err != nil {
						if ctx.Err() != nil {
							return placed
						}
						s.log.Info("import: subtitle not placed", "file", src, "why", err)
						continue
					}
				}
				placed = append(placed, sqlite.HandoffRemoval{Source: src, Library: dest, Identity: removalIdentity(info)})
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
	Note        string              `json:"note,omitempty"`
	Subtitles   int                 `json:"subtitles"`
	Publication *ScannedPublication `json:"publication,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// planContext is what every line of one plan shares, read once.
type planContext struct {
	item      domain.MediaItem
	profile   quality.Profile
	root      string
	disk      sqlite.CopyDiskState
	epStates  map[int64]episodeState
	remove    bool
	lookup    *publicationLookup
	subtitles map[string]map[string]int // folder → video name → count
	downloads map[int64]placeVerdict    // per runner delivery: may it be placed here
	folders   []string                  // library folders (Remove original's refusal)
}

// PlanItemImport answers the confirm step without moving anything: per
// file, the exact destination from the naming template, what happens to a
// file already there (replaced when the new one ranks higher in the profile,
// otherwise kept beside it), how the bytes get there, and every refusal the
// import itself would make. It uses the import's own rules and readers.
func (s *Service) PlanItemImport(ctx context.Context, req ItemImportPlanRequest) ([]ItemImportPlanFile, error) {
	if req.MediaItemID == 0 || len(req.Files) == 0 {
		return nil, fmt.Errorf("%w: an item and at least one file are required", ErrInvalidImportPlan)
	}
	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return nil, err
	}
	pc := &planContext{item: item, root: item.Path, remove: req.RemoveOriginal,
		subtitles: map[string]map[string]int{}, downloads: map[int64]placeVerdict{}}
	profileID := item.QualityProfileID
	if req.CopyID != 0 {
		cp, err := s.db.GetMediaCopy(ctx, item.ID, req.CopyID)
		if err != nil {
			return nil, fmt.Errorf("%w: copy %d is not a copy of this item", ErrInvalidImportPlan, req.CopyID)
		}
		profileID = cp.QualityProfileID
		if cp.Path != "" {
			pc.root = cp.Path
		}
	}
	if pc.root == "" {
		return nil, ErrNoLibraryFolder
	}
	if pc.profile, err = s.db.GetProfile(ctx, profileID); err != nil {
		return nil, err
	}
	if item.Kind == domain.KindSeries {
		if pc.epStates, err = s.episodeStates(ctx, item, req.CopyID); err != nil {
			return nil, err
		}
	}
	if pc.disk, err = s.db.DiskStateForItem(ctx, item.ID, req.CopyID); err != nil {
		return nil, err
	}
	if pc.lookup, err = s.newPublicationLookup(ctx); err != nil {
		return nil, err
	}
	if req.RemoveOriginal {
		if pc.folders, err = s.db.LibraryFolders(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]ItemImportPlanFile, 0, len(req.Files))
	for _, f := range req.Files {
		line, err := s.planItemFile(ctx, pc, f)
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

type placeVerdict struct {
	note    string
	refusal error
}

// verdictFor says whether a runner delivery's files may be placed into this
// item, by the same fence the placement applies: no note for its own item, a
// note when the delivery moves here, a refusal when it may not move.
func (s *Service) verdictFor(ctx context.Context, pc *planContext, downloadID int64) (placeVerdict, error) {
	if v, ok := pc.downloads[downloadID]; ok {
		return v, nil
	}
	note, refusal, err := s.placeVerdict(ctx, pc, downloadID)
	if err != nil {
		return placeVerdict{}, err
	}
	pc.downloads[downloadID] = placeVerdict{note, refusal}
	return pc.downloads[downloadID], nil
}

func (s *Service) placeVerdict(ctx context.Context, pc *planContext, downloadID int64) (note string, refusal error, err error) {
	dl, err := s.db.GetDownload(ctx, downloadID)
	if err != nil {
		return "", nil, err
	}
	if dl.MediaItemID == pc.item.ID {
		return "", nil, nil
	}
	pubs, err := s.db.ListPublicationsForDownload(ctx, downloadID)
	if err != nil {
		return "", nil, err
	}
	var rows []sqlite.PublicationFile
	for _, p := range pubs {
		list, err := s.db.ListPublicationFiles(ctx, p.ID)
		if err != nil {
			return "", nil, err
		}
		rows = append(rows, list...)
	}
	if fence := s.placementFence(ctx, dl, rows); fence != nil {
		if errors.Is(fence, ErrInvalidResolution) {
			return "", fence, nil
		}
		return "", nil, fence
	}
	title := fmt.Sprintf("item %d", dl.MediaItemID)
	if other, err := s.db.GetMediaItemFull(ctx, dl.MediaItemID); err == nil {
		title = other.Title
	}
	return fmt.Sprintf("this runner delivery was grabbed for %s; placing moves it, and its other files, to this item", title), nil, nil
}

func (s *Service) planItemFile(ctx context.Context, pc *planContext, f ItemImportFile) (ItemImportPlanFile, error) {
	item := pc.item
	src := filepath.Clean(f.Path)
	base := filepath.Base(src)
	line := ItemImportPlanFile{Path: src, Season: -1, Episodes: []int{}}
	info, err := os.Lstat(src)
	if err != nil || !info.Mode().IsRegular() {
		line.Error = "not a regular file (a symlink or a folder)"
		if err != nil {
			line.Error = unreadableImportPath(src, err).Error()
		}
		return line, nil
	}
	if !deliveryImportablePath(item.Kind, src) {
		line.Error = "not media of this item's kind"
		return line, nil
	}
	dir := filepath.Dir(src)
	if pc.subtitles[dir] == nil {
		pc.subtitles[dir] = subtitleCounts(dir)
	}
	pub, err := pc.lookup.find(ctx, src)
	if err != nil {
		return line, err
	}
	line.Publication = pub
	if pub != nil {
		switch {
		case pub.DownloadID == 0:
			line.Error = "this runner delivery has no Activity row to place it from"
			return line, nil
		case pub.Outcome == "":
			line.Error = "runner's manifest for this delivery does not name this file, so it cannot be placed"
			return line, nil
		case pub.Outcome != "unresolved" && pub.Outcome != "pending":
			line.Error = "runner's delivery already recorded this file as " + pub.Outcome + "; it cannot be placed again"
			return line, nil
		}
		v, err := s.verdictFor(ctx, pc, pub.DownloadID)
		if err != nil {
			return line, err
		}
		if v.refusal != nil {
			line.Error = strings.TrimPrefix(v.refusal.Error(), ErrInvalidResolution.Error()+": ")
			return line, nil
		}
		line.Note = v.note
	} else {
		line.Subtitles = pc.subtitles[dir][base]
		if pc.remove {
			if why, err := s.removeOriginalRefusal(ctx, src, pc.folders); err != nil {
				return line, err
			} else if why != "" {
				line.Error = "Remove original is refused: " + why
				return line, nil
			}
		}
	}

	parsed := parser.Parse(base)
	var q quality.Quality
	if item.Kind == domain.KindBook {
		q = quality.Quality{Source: quality.Source(filename.BookQualitySource(src))}
	} else {
		q = parsed.Quality
	}
	line.Quality = q.Display()

	var have *quality.Quality
	verified, audio := pc.disk.SourceVerified, pc.disk.Audio
	langs := importLanguages(base, src)
	switch item.Kind {
	case domain.KindSeries:
		season, eps := f.Season, f.Episodes
		if len(eps) == 0 {
			season, eps = nameEpisodes(parsed, base)
		}
		if len(eps) == 0 || season < 0 {
			line.Error = "choose the episode this file is; its name does not say"
			return line, nil
		}
		line.Season, line.Episodes = season, append([]int(nil), eps...)
		title := ""
		for _, ep := range eps {
			id, err := s.db.GetEpisodeID(ctx, item.ID, season, ep)
			if err != nil {
				line.Error = episodeToken(season, []int{ep}) + " is not an episode of this series"
				return line, nil
			}
			st := pc.epStates[id]
			if st.Have != nil && (have == nil || quality.Better(*have, *st.Have)) {
				have, verified, audio = st.Have, st.Verified, st.Audio
			}
			if title == "" {
				title = episodeTitle(item, season, ep)
			}
		}
		line.Destination = episodeDestination(pc.root, item, src, q, season, eps, title)
	case domain.KindBook:
		have = pc.disk.Best
		langs = nil
		line.Destination = filepath.Join(pc.root, naming.BookFileName(item.Author, item.Title)+strings.ToLower(filepath.Ext(src)))
	default:
		have = pc.disk.Best
		line.Destination = movieDestination(pc.root, item, src, q)
	}

	switch {
	case have == nil:
		line.Action, line.Reason = "new", "nothing is there yet"
	case pc.profile.Upgrade(q, langs, *have, verified, audio) || (quality.Better(q, *have) && (item.Kind == domain.KindBook || pc.profile.LanguageAcceptable(langs))):
		line.Action, line.Existing = "replace", have.Display()
		line.Reason = fmt.Sprintf("%s ranks above the %s here in the %s profile; the old file is removed once this one is placed", q.Display(), have.Display(), pc.profile.Name)
	default:
		line.Action, line.Existing = "beside", have.Display()
		line.Reason = fmt.Sprintf("%s does not rank above the %s here in the %s profile; it is kept beside it", q.Display(), have.Display(), pc.profile.Name)
		if _, err := os.Lstat(line.Destination); err == nil {
			line.Reason += " under a [manual-…] name, since a file already has this one"
		}
	}

	switch {
	case pub != nil:
		line.Mode = "runner"
	case sameStorage(src, line.Destination):
		line.Mode = "hardlink"
		if pc.remove {
			line.Mode = "rename"
		}
	default:
		line.Mode = "copy"
		if pc.remove {
			line.Mode = "copy_remove"
		}
	}
	return line, nil
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
// are on one filesystem, so a placement can be a link instead of a copy. A
// variable so tests can stand in for a second volume.
var sameStorage = func(src, dest string) bool {
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

// handoffEpisodeTargets merges the episode choices of every manual entry of
// a download's trace, the newest winning per path.
func handoffEpisodeTargets(handoff []sqlite.HandoffEntry) map[string]sqlite.HandoffEpisodeTarget {
	var out map[string]sqlite.HandoffEpisodeTarget
	for _, entry := range handoff {
		if entry.Step != stepManualQueued {
			continue
		}
		for p, t := range entry.EpisodeTargets {
			if out == nil {
				out = map[string]sqlite.HandoffEpisodeTarget{}
			}
			out[filepath.Clean(p)] = t
		}
	}
	return out
}
