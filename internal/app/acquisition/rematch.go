package acquisition

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/naming"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Re-match (ADR 0027): move files to the right item or episode.
//
// Re-match… on the item page is all of an item's files; Move to… in the Files
// panel is one or more. Both are this one operation. The preview has no side
// effects and lists every rename, the source item's outcome, the releases
// blocklisted and every file that does not fit. Execution takes both titles'
// leases (sorted), refuses a stale preview, journals each move before it
// happens (import_placements, kind rematch_move), moves the bytes (a rename
// on one volume, a verified copy across volumes, with progress and Stop),
// and commits file rows, episode links, history, the blocklist and Cinema's
// outbox in one transaction. A restart resumes from the journal by what it
// observes on disk.

// ErrInvalidRematch is a request that cannot be answered as asked.
var ErrInvalidRematch = errors.New("invalid re-match")

// ErrRematchStalePreview is an execution whose preview no longer describes
// the library: re-preview.
var ErrRematchStalePreview = errors.New("the library changed since this preview; preview again")

// RematchBlockedError is a Re-match refused while work is in flight on
// either title (409 rematch_blocked).
type RematchBlockedError struct{ Blocked RematchBlocked }

func (e *RematchBlockedError) Error() string { return e.Blocked.Message }

// RematchBlocked names what holds a Re-match back, and that waiting helps.
type RematchBlocked struct {
	Reason  string `json:"reason"` // download | import | replacement | rematch
	Ref     string `json:"ref"`
	ItemID  int64  `json:"itemId"`
	Message string `json:"message"`
	Retry   bool   `json:"retry"`
}

// RematchChoice is the person's word on one file: its episode(s) in the
// target, or leave it out.
type RematchChoice struct {
	FileID   int64 `json:"fileId"`
	Season   int   `json:"season"`
	Episodes []int `json:"episodes"`
	Leave    bool  `json:"leave"`
}

// RematchRequest is a preview or an execution.
type RematchRequest struct {
	SourceItemID int64
	// TargetItemID is a library item; NewTarget a hydrated title not yet in
	// the library (preview only: the caller adds it before executing).
	TargetItemID int64
	NewTarget    *domain.MediaItem
	TargetCopyID int64
	FileIDs      []int64 // empty: every file of the source (Re-match…)
	Choices      []RematchChoice
	Blocklist    *bool // default on

	// Execution only.
	RequestID       string
	SourceRevision  int64
	TargetRevision  int64 // 0: the target was added after the preview
	PlanFingerprint string
}

// RematchMovePlan is one line of the preview.
type RematchMovePlan struct {
	FileID   int64         `json:"fileId"`
	From     string        `json:"from"`
	To       string        `json:"to,omitempty"`
	Season   int           `json:"season"`
	Episodes []int         `json:"episodes"`
	Label    string        `json:"label,omitempty"` // S04E06 · Title
	Mode     string        `json:"mode,omitempty"`  // rename | copy | relink
	Bytes    int64         `json:"bytes"`
	Quality  string        `json:"quality,omitempty"`
	Sidecars []RematchPair `json:"sidecars,omitempty"`
	Note     string        `json:"note,omitempty"`
	// Action is what happens to a file the target already has, by the rule
	// import uses (ADR 0027 item 5): new (nothing there), replace (this one
	// ranks higher in the target's profile and its language is acceptable;
	// the old file is removed once this one is committed) or beside.
	Action   string            `json:"action,omitempty"`
	Existing string            `json:"existing,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	Replaces []RematchReplaced `json:"replaces,omitempty"`
	// Problem is why the file is left out: the target has no such episode, a
	// file already stands at the destination, or the person left it out.
	// A left-out file stays on disk and leaves the library.
	Problem   string `json:"problem,omitempty"`
	NeedsPick bool   `json:"needsPick,omitempty"`
	// Leave: the person left it out; only such a file leaves the library.
	// Any other line under leftOut is unresolved: it stays where it is and
	// Move waits until it is picked or left out.
	Leave bool `json:"leave,omitempty"`
}

// RematchReplaced is a target file a move replaces.
type RematchReplaced struct {
	FileID  int64  `json:"fileId"`
	Path    string `json:"path"`
	Quality string `json:"quality,omitempty"`
}

// RematchPair is a sidecar's from → to.
type RematchPair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RematchItemRef names one side of the operation.
type RematchItemRef struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Year      int    `json:"year,omitempty"`
	InLibrary bool   `json:"inLibrary"`
	Folder    string `json:"folder,omitempty"`
	// FolderChosen says this operation chooses the target's folder (with the
	// provider hint) because it has none on disk yet.
	FolderChosen bool `json:"folderChosen,omitempty"`
}

// RematchRelease is a release that will be blocklisted for the source.
type RematchRelease struct {
	FileID  int64  `json:"fileId"`
	Release string `json:"release"`
	Indexer string `json:"indexer,omitempty"`
}

// RematchEpisode is a target episode the picker offers.
type RematchEpisode struct {
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Title   string `json:"title,omitempty"`
	HasFile bool   `json:"hasFile"`
}

// RematchCopy is a target copy or edition the person may move into.
type RematchCopy struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	BookType string `json:"bookType,omitempty"`
}

// RematchPreview is what a Re-match would do, before anything happens.
type RematchPreview struct {
	Source         RematchItemRef    `json:"source"`
	Target         RematchItemRef    `json:"target"`
	TargetCopyID   int64             `json:"targetCopyId"`
	Moves          []RematchMovePlan `json:"moves"`
	LeftOut        []RematchMovePlan `json:"leftOut"`
	SourceOutcome  string            `json:"sourceOutcome"` // stays | removed
	SourceReason   string            `json:"sourceReason"`
	Blocklist      []RematchRelease  `json:"blocklist"`
	BlocklistOn    bool              `json:"blocklistOn"`
	BlocklistNote  string            `json:"blocklistNote,omitempty"`
	Cinema         string            `json:"cinema,omitempty"`
	Episodes       []RematchEpisode  `json:"episodes,omitempty"`
	Copies         []RematchCopy     `json:"copies,omitempty"`
	Blocked        *RematchBlocked   `json:"blocked,omitempty"`
	SourceRevision int64             `json:"sourceRevision"`
	TargetRevision int64             `json:"targetRevision"`
	Fingerprint    string            `json:"fingerprint"`
	// Unresolved counts leftOut lines that need a choice; Move refuses
	// while it is not 0.
	Unresolved int    `json:"unresolved"`
	Summary    string `json:"summary"`
}

// rematchPlan is a preview plus what execution needs that the page does not.
type rematchPlan struct {
	preview    RematchPreview
	source     domain.MediaItem
	target     domain.MediaItem
	targetPath string // set when this operation chooses the target's folder
	files      map[int64]domain.MediaFile
	epIDs      map[int64][]int64 // file → target episode ids
	removeSrc  bool
}

const rematchLeaseWait = 10 * time.Second

// PreviewRematch answers what a Re-match would do, without side effects.
func (s *Service) PreviewRematch(ctx context.Context, req RematchRequest) (RematchPreview, error) {
	plan, err := s.planRematch(ctx, req)
	if err != nil {
		return RematchPreview{}, err
	}
	return plan.preview, nil
}

func (s *Service) planRematch(ctx context.Context, req RematchRequest) (*rematchPlan, error) {
	source, err := s.db.GetMediaItemFull(ctx, req.SourceItemID)
	if err != nil {
		return nil, err
	}
	var target domain.MediaItem
	inLibrary := true
	switch {
	case req.TargetItemID != 0:
		if target, err = s.db.GetMediaItemFull(ctx, req.TargetItemID); err != nil {
			return nil, err
		}
	case req.NewTarget != nil:
		if id := s.LibraryItemFor(ctx, *req.NewTarget); id != 0 {
			if target, err = s.db.GetMediaItemFull(ctx, id); err != nil {
				return nil, err
			}
		} else {
			target, inLibrary = *req.NewTarget, false
			target.ID = 0
		}
	default:
		return nil, fmt.Errorf("%w: choose the title the files belong to", ErrInvalidRematch)
	}
	if target.Kind != source.Kind {
		return nil, fmt.Errorf("%w: a %s's files can only move to another %s", ErrInvalidRematch, source.Kind, source.Kind)
	}
	plan := &rematchPlan{source: source, target: target, files: map[int64]domain.MediaFile{}, epIDs: map[int64][]int64{}}
	p := &plan.preview
	p.Source = RematchItemRef{ID: source.ID, Kind: string(source.Kind), Title: source.Title, Year: source.Year, InLibrary: true, Folder: source.Path}
	p.Target = RematchItemRef{ID: target.ID, Kind: string(target.Kind), Title: target.Title, Year: target.Year, InLibrary: inLibrary, Folder: target.Path}
	p.TargetCopyID = req.TargetCopyID
	p.Moves, p.LeftOut, p.Blocklist = []RematchMovePlan{}, []RematchMovePlan{}, []RematchRelease{}

	// The files that move.
	selected := map[int64]bool{}
	for _, id := range req.FileIDs {
		selected[id] = true
	}
	var moving []domain.MediaFile
	for _, f := range source.Files {
		if len(selected) == 0 || selected[f.ID] {
			moving = append(moving, f)
		}
	}
	if len(selected) > 0 && len(moving) != len(selected) {
		return nil, fmt.Errorf("%w: a chosen file does not belong to %s", ErrInvalidRematch, source.Title)
	}
	if source.Kind == domain.KindBook && len(selected) > 0 {
		// The parts of a multipart audiobook move as one unit.
		copies := map[int64]bool{}
		for _, f := range moving {
			copies[f.CopyID] = true
		}
		moving = moving[:0]
		for _, f := range source.Files {
			if copies[f.CopyID] {
				moving = append(moving, f)
			}
		}
	}
	if len(moving) == 0 {
		return nil, fmt.Errorf("%w: %s has no files to move", ErrInvalidRematch, source.Title)
	}
	sort.Slice(moving, func(i, j int) bool { return moving[i].Path < moving[j].Path })

	// Where they go: the copy's folder, or the item's. A target with no
	// folder on disk and no files gets one chosen here, with the provider
	// hint, so the next scan or Cinema match keeps the pairing (ADR 0020).
	root := target.Path
	if req.TargetCopyID != 0 {
		var cp *domain.MediaCopy
		for i := range target.Copies {
			if target.Copies[i].ID == req.TargetCopyID {
				cp = &target.Copies[i]
			}
		}
		if cp == nil {
			return nil, fmt.Errorf("%w: copy %d is not a copy of %s", ErrInvalidRematch, req.TargetCopyID, target.Title)
		}
		if cp.Path != "" {
			root = cp.Path
		}
	}
	for _, cp := range target.Copies {
		p.Copies = append(p.Copies, RematchCopy{ID: cp.ID, Name: cp.Name, BookType: string(cp.BookType)})
	}
	if source.Kind == domain.KindBook {
		if err := rematchBookEditions(source, target, moving, req.TargetCopyID); err != nil {
			return nil, err
		}
	}
	targetHasFiles := false
	for _, f := range target.Files {
		if f.CopyID == req.TargetCopyID {
			targetHasFiles = true
		}
	}
	if req.TargetCopyID == 0 && !targetHasFiles && (root == "" || !dirExists(root)) {
		chosen, err := s.chooseRematchFolder(ctx, source, target)
		if err != nil {
			return nil, err
		}
		root, plan.targetPath = chosen, chosen
		p.Target.Folder, p.Target.FolderChosen = chosen, true
	}
	if root == "" {
		return nil, ErrNoLibraryFolder
	}

	// One line per file.
	sourceEpisodes := episodeIndex(source)
	choices := map[int64]RematchChoice{}
	for _, c := range req.Choices {
		choices[c.FileID] = c
	}
	claimed := map[string]bool{}
	rank := &rematchRank{target: target, copyID: req.TargetCopyID}
	for i, f := range moving {
		plan.files[f.ID] = f
		line := RematchMovePlan{FileID: f.ID, From: f.Path, Season: -1, Episodes: []int{}, Bytes: f.Size, Quality: f.Quality.Display()}
		choice, chosen := choices[f.ID]
		if chosen && choice.Leave {
			line.Leave = true
			line.Problem = "left out: it stays on disk at " + f.Path + " and leaves the library"
			p.LeftOut = append(p.LeftOut, line)
			continue
		}
		switch target.Kind {
		case domain.KindSeries:
			season, eps := -1, []int(nil)
			if chosen && len(choice.Episodes) > 0 {
				season, eps = choice.Season, choice.Episodes
			} else {
				for _, id := range f.EpisodeIDs {
					if e, ok := sourceEpisodes[id]; ok {
						season = e.SeasonNumber
						eps = append(eps, e.EpisodeNumber)
					}
				}
				sort.Ints(eps)
			}
			line.Season, line.Episodes = season, append([]int{}, eps...)
			if len(eps) == 0 {
				line.Problem, line.NeedsPick = "this file has no episode; choose one or leave it out", true
				p.LeftOut = append(p.LeftOut, line)
				continue
			}
			var ids []int64
			var missing []string
			title, hasFile := "", false
			for _, n := range eps {
				e, ok := findEpisode(target, season, n)
				if !ok {
					missing = append(missing, episodeToken(season, []int{n}))
					continue
				}
				ids = append(ids, e.ID)
				if title == "" {
					title = e.Title
				}
				hasFile = hasFile || targetEpisodeHasFile(target, e.ID, f.ID)
			}
			if len(missing) > 0 {
				line.Problem = fmt.Sprintf("%s has no %s; choose an episode or leave it out", target.Title, strings.Join(missing, ", "))
				line.NeedsPick = true
				p.LeftOut = append(p.LeftOut, line)
				continue
			}
			line.Label = episodeToken(season, eps)
			if title != "" {
				line.Label += " · " + title
			}
			line.To = episodeDestination(root, target, f.Path, f.Quality, season, eps, title)
			plan.epIDs[f.ID] = ids
		case domain.KindBook:
			base := naming.BookFileName(target.Author, target.Title)
			if len(moving) > 1 {
				base += fmt.Sprintf(" - %03d", i+1)
			}
			line.To = filepath.Join(root, base+strings.ToLower(filepath.Ext(f.Path)))
		default:
			line.To = movieDestination(root, target, f.Path, f.Quality)
		}
		if problem := s.rematchDestinationProblem(ctx, f, line.To, claimed); problem != "" {
			line.Problem, line.NeedsPick = problem, target.Kind == domain.KindSeries
			line.To = ""
			p.LeftOut = append(p.LeftOut, line)
			continue
		}
		claimed[line.To] = true
		if err := s.rematchVerdict(ctx, rank, &line, f, moving, plan.epIDs[f.ID]); err != nil {
			return nil, err
		}
		switch {
		case line.To == f.Path:
			line.Mode = "relink"
		case sameStorage(f.Path, line.To):
			line.Mode = "rename"
		default:
			line.Mode = "copy"
		}
		line.Sidecars = s.rematchSidecars(ctx, f.Path, line.To)
		p.Moves = append(p.Moves, line)
	}
	for i := range p.LeftOut {
		if l := &p.LeftOut[i]; !l.Leave {
			p.Unresolved++
			l.Problem += "; until you choose, it stays where it is and nothing moves"
		}
	}
	if target.Kind == domain.KindSeries {
		for _, se := range target.Seasons {
			for _, e := range se.Episodes {
				p.Episodes = append(p.Episodes, RematchEpisode{Season: se.Number, Episode: e.EpisodeNumber, Title: e.Title, HasFile: targetEpisodeHasFile(target, e.ID, 0)})
			}
		}
	}

	// What happens to the item the files leave.
	if err := s.rematchSourceOutcome(ctx, plan, moving); err != nil {
		return nil, err
	}

	// The releases behind the moved files are blocklisted for it.
	p.BlocklistOn = req.Blocklist == nil || *req.Blocklist
	stable := 0
	seen := map[string]bool{}
	for _, mv := range p.Moves {
		f := plan.files[mv.FileID]
		if f.SourceGUID == "" || f.SourceIndexerID == 0 {
			continue
		}
		stable++
		key := f.SourceGUID + "\x00" + strconv.FormatInt(f.SourceIndexerID, 10)
		if seen[key] || plan.removeSrc || (source.ID == target.ID && source.Kind != domain.KindSeries) {
			continue
		}
		seen[key] = true
		release := f.SourceRelease
		if release == "" {
			release = f.SourceGUID
		}
		p.Blocklist = append(p.Blocklist, RematchRelease{FileID: f.ID, Release: release, Indexer: f.SourceIndexer})
	}
	switch {
	case plan.removeSrc && stable > 0:
		p.BlocklistNote = source.Title + " is removed, so there is nothing to blocklist the release for"
	case stable == 0 && len(p.Moves) > 0:
		p.BlocklistNote = "These files were adopted from disk, not grabbed, so there is no release to blocklist"
	case len(p.Blocklist) > 0 && source.Kind == domain.KindSeries:
		p.BlocklistNote = "Blocklisted for " + source.Title + ", so automatic search does not grab the same release for the episode it left"
	case len(p.Blocklist) > 0:
		p.BlocklistNote = "Blocklisted for " + source.Title + ", so automatic search looks for a real copy of it"
	}

	// Cinema: an older one cannot drop the old entry.
	notifiers, err := s.db.ListNotifiers(ctx)
	if err != nil {
		return nil, err
	}
	for _, cfg := range notifiers {
		if cfg.Enabled && cfg.OnImport && ports.QueuedNotifier(cfg) {
			if !cfg.HasFeature(ports.FeatureRemovedPaths) {
				p.Cinema = cfg.Name + " does not support removed paths: it scans the new folder, and the old entry stays in cinema until its next reconcile scan"
				break
			}
			p.Cinema = cfg.Name + " scans the new folder and drops the old entry"
		}
	}

	// Fences and leases.
	ids := []int64{source.ID}
	if target.ID != 0 && target.ID != source.ID {
		ids = append(ids, target.ID)
	}
	blockers, err := s.db.RematchBlockers(ctx, ids...)
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		b := blockers[0]
		p.Blocked = &RematchBlocked{Reason: b.Reason, Ref: b.Ref, ItemID: b.ItemID, Retry: true, Message: rematchBlockedText(b, source, target)}
	}
	if p.SourceRevision, err = s.db.ItemRevision(ctx, source.ID); err != nil {
		return nil, err
	}
	if target.ID != 0 {
		if p.TargetRevision, err = s.db.ItemRevision(ctx, target.ID); err != nil {
			return nil, err
		}
	}
	p.Fingerprint = rematchFingerprint(plan)
	n := len(p.Moves)
	p.Summary = fmt.Sprintf("Move %d file%s", n, pluralS(n))
	return plan, nil
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// LibraryItemFor finds a hydrated title already in the library.
func (s *Service) LibraryItemFor(ctx context.Context, item domain.MediaItem) int64 {
	if item.IDs.TMDB != 0 {
		if id, err := s.db.GetMediaItemByKindTmdb(ctx, item.Kind, item.IDs.TMDB); err == nil {
			return id
		}
	}
	if item.IDs.TVDB != 0 {
		if id, err := s.db.GetMediaItemByKindTvdb(ctx, item.Kind, item.IDs.TVDB); err == nil {
			return id
		}
	}
	if item.IDs.OLID != "" {
		if id, err := s.db.GetMediaItemByKindOlid(ctx, item.Kind, item.IDs.OLID); err == nil {
			return id
		}
	}
	return 0
}

// RematchRootFolder is the root a title Re-match adds goes under: the source's
// own root when it takes this kind, else the first root that does.
func (s *Service) RematchRootFolder(ctx context.Context, source domain.MediaItem, kind domain.MediaKind) (domain.RootFolder, error) {
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return domain.RootFolder{}, err
	}
	for _, r := range roots {
		if r.ID == source.RootFolderID && r.Kind.Accepts(kind) {
			return r, nil
		}
	}
	for _, r := range roots {
		if r.Kind.Accepts(kind) {
			return r, nil
		}
	}
	return domain.RootFolder{}, ErrNoLibraryFolder
}

// rematchFolderTag is the provider hint a folder this operation chooses
// always carries (ADR 0027 item 6; the order matches ADR 0020's).
func rematchFolderTag(item domain.MediaItem) string {
	switch {
	case item.IDs.TMDB != 0:
		return naming.FolderTag("tmdb", strconv.FormatInt(item.IDs.TMDB, 10))
	case item.IDs.TVDB != 0:
		return naming.FolderTag("tvdb", strconv.FormatInt(item.IDs.TVDB, 10))
	case item.IDs.IMDB != "":
		return naming.FolderTag("imdb", item.IDs.IMDB)
	case item.IDs.OLID != "":
		return naming.FolderTag("olid", item.IDs.OLID)
	case item.ID != 0:
		return naming.FolderTag("monarr", strconv.FormatInt(item.ID, 10))
	}
	return ""
}

func (s *Service) chooseRematchFolder(ctx context.Context, source, target domain.MediaItem) (string, error) {
	rootPath := ""
	if target.RootFolderID != 0 {
		if rf, err := s.db.GetRootFolder(ctx, target.RootFolderID); err == nil {
			rootPath = rf.Path
		}
	}
	if rootPath == "" {
		rf, err := s.RematchRootFolder(ctx, source, target.Kind)
		if err != nil {
			return "", err
		}
		rootPath = rf.Path
	}
	base := filepath.Join(rootPath, naming.FolderName(target.Title, target.Year))
	if target.Kind == domain.KindBook {
		base = filepath.Join(rootPath, naming.BookFolder(target.Author, target.Title))
	}
	if tag := rematchFolderTag(target); tag != "" {
		base += " " + tag
	}
	claims, err := s.db.ListFolderClaims(ctx)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, c := range claims {
		if c.ItemID != target.ID || target.ID == 0 {
			taken[c.Path] = true
		}
	}
	candidate := base
	for n := 2; taken[candidate] || (n > 2 && dirExists(candidate)); n++ {
		candidate = fmt.Sprintf("%s (%d)", base, n)
	}
	return candidate, nil
}

func episodeIndex(item domain.MediaItem) map[int64]domain.Episode {
	out := map[int64]domain.Episode{}
	for _, se := range item.Seasons {
		for _, e := range se.Episodes {
			if e.SeasonNumber == 0 && se.Number != 0 {
				e.SeasonNumber = se.Number
			}
			out[e.ID] = e
		}
	}
	return out
}

func findEpisode(item domain.MediaItem, season, ep int) (domain.Episode, bool) {
	for _, se := range item.Seasons {
		if se.Number != season {
			continue
		}
		for _, e := range se.Episodes {
			if e.EpisodeNumber == ep {
				return e, true
			}
		}
	}
	return domain.Episode{}, false
}

func targetEpisodeHasFile(item domain.MediaItem, episodeID, except int64) bool {
	for _, f := range item.Files {
		if f.ID == except {
			continue
		}
		for _, id := range f.EpisodeIDs {
			if id == episodeID {
				return true
			}
		}
	}
	return false
}

// rematchBookEditions keeps a book move between editions of one medium
// (ADR 0018).
func rematchBookEditions(source, target domain.MediaItem, moving []domain.MediaFile, targetCopy int64) error {
	medium := func(item domain.MediaItem, copyID int64) string {
		if copyID == 0 {
			return string(item.BookType)
		}
		for _, c := range item.Copies {
			if c.ID == copyID {
				return string(c.BookType)
			}
		}
		return ""
	}
	want := medium(target, targetCopy)
	for _, f := range moving {
		if have := medium(source, f.CopyID); have != "" && want != "" && have != want {
			return fmt.Errorf("%w: a %s moves only to an %s edition; choose the %s edition of %s", ErrInvalidRematch, have, have, have, target.Title)
		}
	}
	return nil
}

// rematchDestinationProblem refuses a destination that is taken: never
// overwrite (ADR 0027 item 5).
func (s *Service) rematchDestinationProblem(ctx context.Context, f domain.MediaFile, dest string, claimed map[string]bool) string {
	if dest == f.Path {
		return ""
	}
	if claimed[dest] {
		return "another moved file already goes to " + dest
	}
	if _, err := os.Lstat(dest); err == nil {
		return "a file already exists at " + dest
	}
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_files WHERE path=?`, dest).Scan(&n); err == nil && n > 0 {
		return "the library already has a file at " + dest
	}
	return ""
}

// rematchSidecars lists the files beside src that share its stem (subtitles,
// .nfo): they travel with it, renamed to the new stem.
func (s *Service) rematchSidecars(ctx context.Context, src, dest string) []RematchPair {
	if src == dest {
		return nil
	}
	dir := filepath.Dir(src)
	stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	newStem := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []RematchPair
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || name == filepath.Base(src) || !strings.HasPrefix(name, stem+".") {
			continue
		}
		// Another video or book that shares the stem ("Dune (1984).1080p.mkv"
		// beside "Dune (1984).mkv") is media in its own right, never a
		// sidecar; so is anything the library holds as a file.
		full := filepath.Join(dir, name)
		if filename.IsVideo(name) || filename.IsBook(name) || s.libraryHasPath(ctx, full) {
			continue
		}
		out = append(out, RematchPair{From: filepath.Join(dir, name), To: filepath.Join(filepath.Dir(dest), newStem+strings.TrimPrefix(name, stem))})
	}
	return out
}

func (s *Service) libraryHasPath(ctx context.Context, path string) bool {
	var n int
	if err := s.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_files WHERE path=?`, path).Scan(&n); err != nil {
		return true // unknown: do not carry it along
	}
	return n > 0
}

// rematchRank is what the replace-or-beside rule reads about the target,
// once per preview: the same readers import uses.
type rematchRank struct {
	target  domain.MediaItem
	copyID  int64
	ready   bool
	profile quality.Profile
	disk    sqlite.CopyDiskState
	eps     map[int64]episodeState
}

func (r *rematchRank) load(ctx context.Context, s *Service) error {
	if r.ready {
		return nil
	}
	r.ready = true
	profileID := r.target.QualityProfileID
	for _, c := range r.target.Copies {
		if c.ID == r.copyID && r.copyID != 0 {
			profileID = c.QualityProfileID
		}
	}
	if profileID == 0 {
		profileID = s.db.DefaultProfileID(ctx, r.target.Kind)
		if r.target.Kind == domain.KindBook {
			profileID = s.db.DefaultBookProfileID(ctx, r.target.BookType)
		}
	}
	var err error
	if r.profile, err = s.db.GetProfile(ctx, profileID); err != nil {
		return err
	}
	if r.target.Kind == domain.KindSeries {
		r.eps, err = s.episodeStates(ctx, r.target, r.copyID)
		return err
	}
	r.disk, err = s.db.DiskStateForItem(ctx, r.target.ID, r.copyID)
	return err
}

// rematchVerdict applies import's rule to a moved file meeting a file the
// target already has (PlanItemImport): it replaces it when it ranks higher in
// the target's profile and its language is acceptable, else it is kept
// beside it. The files it replaces are the ones import's removeExistingFiles
// removes: the same copy's (a series: those on the same episodes).
func (s *Service) rematchVerdict(ctx context.Context, r *rematchRank, line *RematchMovePlan, f domain.MediaFile, moving []domain.MediaFile, epIDs []int64) error {
	line.Action = "new"
	if r.target.ID == 0 {
		return nil
	}
	movingIDs := map[int64]bool{}
	for _, m := range moving {
		movingIDs[m.ID] = true
	}
	covered := map[int64]bool{}
	for _, id := range epIDs {
		covered[id] = true
	}
	var others []domain.MediaFile
	for _, o := range r.target.Files {
		if movingIDs[o.ID] || o.CopyID != r.copyID {
			continue
		}
		if r.target.Kind == domain.KindSeries {
			hit := false
			for _, id := range o.EpisodeIDs {
				hit = hit || covered[id]
			}
			if !hit {
				continue
			}
		}
		others = append(others, o)
	}
	if len(others) == 0 {
		return nil
	}
	if err := r.load(ctx, s); err != nil {
		return err
	}
	var have *quality.Quality
	verified, audio := r.disk.SourceVerified, r.disk.Audio
	langs := importLanguages(f.SourceRelease, f.Path)
	switch r.target.Kind {
	case domain.KindSeries:
		for _, id := range epIDs {
			st := r.eps[id]
			if st.Have != nil && (have == nil || quality.Better(*have, *st.Have)) {
				have, verified, audio = st.Have, st.Verified, st.Audio
			}
		}
	case domain.KindBook:
		have, langs = r.disk.Best, nil
	default:
		have = r.disk.Best
	}
	q := f.Quality
	switch {
	case have == nil:
		line.Action, line.Reason = "beside", "the file already here has no known quality; this one is kept beside it"
	case r.profile.Upgrade(q, langs, *have, verified, audio) || (quality.Better(q, *have) && (r.target.Kind == domain.KindBook || r.profile.LanguageAcceptable(langs))):
		line.Action, line.Existing = "replace", have.Display()
		line.Reason = fmt.Sprintf("%s ranks above the %s here in the %s profile; the old file is removed once this one is in place", q.Display(), have.Display(), r.profile.Name)
		for _, o := range others {
			line.Replaces = append(line.Replaces, RematchReplaced{FileID: o.ID, Path: o.Path, Quality: o.Quality.Display()})
		}
	default:
		line.Action, line.Existing = "beside", have.Display()
		line.Reason = fmt.Sprintf("%s does not rank above the %s here in the %s profile; it is kept beside it", q.Display(), have.Display(), r.profile.Name)
	}
	return nil
}

// rematchSourceOutcome decides whether the item the files leave stays (it
// was added by a person, or still holds something) or is removed (folder
// adoption created it and it is now empty).
func (s *Service) rematchSourceOutcome(ctx context.Context, plan *rematchPlan, moving []domain.MediaFile) error {
	p := &plan.preview
	source := plan.source
	p.SourceOutcome = "stays"
	if source.ID == plan.target.ID {
		p.SourceReason = "the files stay in " + source.Title
		return nil
	}
	origin, err := s.db.MediaItemOrigin(ctx, source.ID)
	if err != nil {
		return err
	}
	remaining := len(source.Files) - len(moving)
	downloads, err := s.db.ItemDownloadCount(ctx, source.ID)
	if err != nil {
		return err
	}
	switch {
	case origin != "adopted":
		p.SourceReason = "stays in the library as wanted: you added it"
	case remaining > 0:
		p.SourceReason = fmt.Sprintf("stays in the library: it still has %d file%s", remaining, pluralS(remaining))
	case downloads > 0:
		p.SourceReason = "stays in the library: it has downloads in Activity"
	default:
		p.SourceOutcome, plan.removeSrc = "removed", true
		p.SourceReason = "removed: folder adoption created it and it is now empty"
	}
	return nil
}

func rematchBlockedText(b sqlite.RematchBlocker, source, target domain.MediaItem) string {
	title := source.Title
	if b.ItemID == target.ID && target.ID != source.ID {
		title = target.Title
	}
	switch b.Reason {
	case "download":
		return fmt.Sprintf("%s has a download in flight (Activity %s, %s); wait for it to finish", title, b.Ref, b.State)
	case "import":
		return fmt.Sprintf("%s is being imported (Activity %s); wait for it to finish", title, b.Ref)
	case "replacement":
		switch b.State {
		case "waiting_for_release", "cleanup_blocked", "searching":
			// These can last indefinitely, and replacements have no cancel:
			// say so rather than promise that waiting ends it.
			return fmt.Sprintf("%s has a replacement %s (%s); it holds the title until it completes or fails, and a replacement cannot be cancelled — wait for it, or move the files once it is done", title, strings.ReplaceAll(b.State, "_", " "), b.Ref)
		}
		return fmt.Sprintf("%s has a replacement in progress (%s); wait for it to finish", title, b.State)
	case "offload":
		return fmt.Sprintf("%s is being offloaded (%s); wait for it to finish, or Stop it", title, b.State)
	default:
		return fmt.Sprintf("%s has another re-match in progress; wait for it to finish", title)
	}
}

func rematchFingerprint(plan *rematchPlan) string {
	p := plan.preview
	h := sha256.New()
	ids := plan.target.IDs
	fmt.Fprintf(h, "%d|%s|%d|%d|%s|%s|%d|%v|%v\n", p.Source.ID, plan.target.Kind, ids.TMDB, ids.TVDB, ids.IMDB, ids.OLID, p.TargetCopyID, p.BlocklistOn, plan.removeSrc)
	if plan.target.Source == domain.SourceManual || (ids.TMDB == 0 && ids.TVDB == 0 && ids.IMDB == "" && ids.OLID == "") {
		fmt.Fprintf(h, "item %d\n", plan.target.ID)
	}
	for _, m := range p.Moves {
		fmt.Fprintf(h, "m %d %s %s %d %v %s %s", m.FileID, m.From, m.To, m.Season, m.Episodes, m.Mode, m.Action)
		for _, r := range m.Replaces {
			fmt.Fprintf(h, " r%d", r.FileID)
		}
		for _, sc := range m.Sidecars {
			fmt.Fprintf(h, " s%s>%s", sc.From, sc.To)
		}
		_, _ = fmt.Fprintln(h)
	}
	for _, m := range p.LeftOut {
		fmt.Fprintf(h, "l %d %s %v\n", m.FileID, m.From, m.Leave)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// StartRematch executes a previewed Re-match. It returns once the operation
// is recorded; the moves run in the background (GET /rematch/{id}), admitted
// as work so a deployment drain sees and waits for them.
func (s *Service) StartRematch(ctx context.Context, req RematchRequest) (sqlite.RematchOperation, error) {
	if req.RequestID == "" || req.PlanFingerprint == "" {
		return sqlite.RematchOperation{}, fmt.Errorf("%w: requestId and the preview's fingerprint are required", ErrInvalidRematch)
	}
	if req.TargetItemID == 0 {
		return sqlite.RematchOperation{}, fmt.Errorf("%w: execute against a library item", ErrInvalidRematch)
	}
	requestFingerprint := req.PlanFingerprint
	// The request id is checked first, so a retried request answers with the
	// operation it created.
	if op, err := s.rematchByRequest(ctx, req.RequestID); err == nil {
		if op.Fingerprint != requestFingerprint {
			return sqlite.RematchOperation{}, sqlite.ErrRematchRequestConflict
		}
		return op, nil
	}
	workCtx, releaseWork, err := ports.TryWork(s.rematchBase(), s.admission, "rematch")
	if err != nil {
		return sqlite.RematchOperation{}, err
	}
	leaseCtx, release, err := s.lockImportTargets(workCtx, rematchLeaseWait, req.SourceItemID, req.TargetItemID)
	if err != nil {
		releaseWork()
		return sqlite.RematchOperation{}, err
	}
	op, err := s.startRematchLocked(leaseCtx, req, requestFingerprint)
	if err != nil {
		release()
		releaseWork()
		return sqlite.RematchOperation{}, err
	}
	s.launchRematch(leaseCtx, op.ID, func() { release(); releaseWork() })
	return op, nil
}

// rematchBase is the context background moves run under: the importers'
// lifetime once they started (a shutdown stops a copy; the journal resumes
// it), else the process's.
func (s *Service) rematchBase() context.Context {
	s.rematchMu.Lock()
	defer s.rematchMu.Unlock()
	if s.rematchCtx != nil {
		return s.rematchCtx
	}
	return context.Background()
}

// launchRematch drives one operation in the background, once: an operation
// this process is already driving is left to that goroutine. release frees
// the titles and the admission token when it ends.
func (s *Service) launchRematch(ctx context.Context, id string, release func()) bool {
	s.rematchMu.Lock()
	if s.rematchCancels == nil {
		s.rematchCancels = map[string]context.CancelFunc{}
	}
	if _, running := s.rematchCancels[id]; running {
		s.rematchMu.Unlock()
		release()
		return false
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.rematchCancels[id] = cancel
	s.rematchMu.Unlock()
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		defer release()
		defer func() {
			s.rematchMu.Lock()
			delete(s.rematchCancels, id)
			s.rematchMu.Unlock()
			cancel()
		}()
		if err := s.runRematch(runCtx, id); err != nil {
			s.log.Warn("re-match is not finished; it is retried", "operation", id, "err", err)
			_ = s.db.SetRematchError(context.WithoutCancel(runCtx), id, err.Error())
		}
	}()
	return true
}

func (s *Service) rematchRunning(id string) bool {
	s.rematchMu.Lock()
	defer s.rematchMu.Unlock()
	_, ok := s.rematchCancels[id]
	return ok
}

func (s *Service) rematchByRequest(ctx context.Context, requestID string) (sqlite.RematchOperation, error) {
	var id string
	if err := s.db.R.QueryRowContext(ctx, `SELECT operation_id FROM rematch_operations WHERE request_id=?`, requestID).Scan(&id); err != nil {
		return sqlite.RematchOperation{}, err
	}
	return s.db.GetRematchOperation(ctx, id)
}

func (s *Service) startRematchLocked(ctx context.Context, req RematchRequest, fingerprint string) (sqlite.RematchOperation, error) {
	plan, err := s.planRematch(ctx, req)
	if err != nil {
		return sqlite.RematchOperation{}, err
	}
	p := plan.preview
	if p.Blocked != nil {
		return sqlite.RematchOperation{}, &RematchBlockedError{Blocked: *p.Blocked}
	}
	if p.SourceRevision != req.SourceRevision || (req.TargetRevision != 0 && p.TargetRevision != req.TargetRevision) || p.Fingerprint != req.PlanFingerprint {
		return sqlite.RematchOperation{}, ErrRematchStalePreview
	}
	if p.Unresolved > 0 {
		return sqlite.RematchOperation{}, fmt.Errorf("%w: %d file(s) need a choice (an episode, or Leave it out) before anything moves", ErrInvalidRematch, p.Unresolved)
	}
	if len(p.Moves) == 0 {
		return sqlite.RematchOperation{}, fmt.Errorf("%w: nothing to move", ErrInvalidRematch)
	}
	var copyBytes int64
	byVolume := map[string]int64{}
	for _, m := range p.Moves {
		if m.Mode == "copy" {
			copyBytes += m.Bytes
			byVolume[filepath.Dir(m.To)] += m.Bytes
		}
	}
	for dir, n := range byVolume {
		if err := recoveryCapacity(dir, n); err != nil {
			return sqlite.RematchOperation{}, fmt.Errorf("%w: %v", ErrInvalidRematch, err)
		}
	}
	opID := newRematchID()
	raw, err := json.Marshal(rematchStored{Request: req, Preview: p, TargetPath: plan.targetPath, EpisodeIDs: plan.epIDs, RemoveSource: plan.removeSrc})
	if err != nil {
		return sqlite.RematchOperation{}, err
	}
	var moves []sqlite.RematchMove
	for i, m := range p.Moves {
		f := plan.files[m.FileID]
		identity, err := rematchIdentity(f.Path)
		if err != nil {
			return sqlite.RematchOperation{}, fmt.Errorf("%w: %s cannot be read: %v", ErrInvalidRematch, f.Path, err)
		}
		mv := sqlite.RematchMove{ID: fmt.Sprintf("rematch:%s:%03d", opID, i), OperationID: opID, FileID: f.ID, ItemID: plan.source.ID,
			Source: f.Path, Target: m.To, Mode: m.Mode, Identity: identity, Size: f.Size, State: "rematch_planned",
			Replace: m.Action == "replace", CopyID: p.TargetCopyID, EpisodeIDs: plan.epIDs[f.ID]}
		for _, r := range m.Replaces {
			mv.ReplaceIDs = append(mv.ReplaceIDs, r.FileID)
		}
		if m.Mode == "copy" {
			mv.Temporary = filepath.Join(filepath.Dir(m.To), "."+filepath.Base(m.To)+".rematch-"+opID+".partial")
		}
		for _, sc := range m.Sidecars {
			mv.Sidecars = append(mv.Sidecars, [2]string{sc.From, sc.To})
		}
		moves = append(moves, mv)
	}
	op, existing, err := s.db.CreateRematchOperation(ctx, sqlite.RematchOperation{ID: opID, RequestID: req.RequestID, Fingerprint: fingerprint,
		State: "planned", SourceItemID: plan.source.ID, TargetItemID: req.TargetItemID, Plan: raw, BytesTotal: copyBytes}, moves)
	if err != nil {
		return op, err
	}
	if existing {
		return op, nil
	}
	s.log.Info("re-match started", "operation", opID, "from", plan.source.Title, "to", plan.target.Title, "files", len(p.Moves))
	return op, nil
}

// rematchStored is the plan an operation executes (rematch_operations.plan).
type rematchStored struct {
	Request      RematchRequest    `json:"request"`
	Preview      RematchPreview    `json:"preview"`
	TargetPath   string            `json:"targetPath,omitempty"`
	EpisodeIDs   map[int64][]int64 `json:"episodeIds,omitempty"`
	RemoveSource bool              `json:"removeSource"`
}

func newRematchID() string {
	var b [8]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	return "rm-" + hex.EncodeToString(b[:])
}

// rematchIdentity is what resume compares to decide a source is unchanged:
// device, inode, size and mtime.
func rematchIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	return fileIdentityString(info), nil
}

func fileIdentityString(info os.FileInfo) string {
	dev, _ := fileDevice(info)
	return fmt.Sprintf("%d:%v:%d:%d", dev, recoveryDirectoryIdentity(info), info.Size(), info.ModTime().UnixNano())
}

// inodeOf drops size and mtime: a rename keeps the inode.
func inodeOf(identity string) string {
	parts := strings.SplitN(identity, ":", 3)
	if len(parts) < 2 {
		return identity
	}
	return parts[0] + ":" + parts[1]
}

// CancelRematch is Stop: an operation still moving is rolled back (a copy's
// temp file removed, the source untouched); past commit it cannot be.
func (s *Service) CancelRematch(ctx context.Context, id string) error {
	ok, err := s.db.RequestRematchCancel(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := s.db.GetRematchOperation(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("%w: the files are already in place; it can no longer be stopped", ErrInvalidRematch)
	}
	s.rematchMu.Lock()
	cancel := s.rematchCancels[id]
	s.rematchMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// DismissRematch hides a finished operation from the item pages' status
// panels. A running operation cannot be dismissed.
func (s *Service) DismissRematch(ctx context.Context, id string) error {
	ok, err := s.db.DismissRematch(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := s.db.GetRematchOperation(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("%w: it is still running; Stop it instead", ErrInvalidRematch)
	}
	return nil
}

// RematchStatus is GET /rematch/{id}: the operation and what Cinema said.
type RematchStatus struct {
	sqlite.RematchOperation
	Cinema []RematchCinema `json:"cinema"`
}

// RematchCinema is one Cinema sub-delivery of the operation.
type RematchCinema struct {
	Directory string `json:"directory"`
	Status    string `json:"status"`
	Result    string `json:"result,omitempty"`
	LastError string `json:"lastError,omitempty"`
	VisibleAt int64  `json:"visibleAt,omitempty"`
	// Warnings are cinema's lasting answers about removed paths, read from
	// the delivery's structured file results.
	Warnings []string `json:"warnings,omitempty"`
}

// RematchCinemaOf is the operation's view of one cinema sub-delivery.
func RematchCinemaOf(d sqlite.Delivery) RematchCinema {
	return RematchCinema{Directory: d.Directory, Status: d.Status, Result: d.Result, LastError: d.LastError, VisibleAt: d.VisibleAt, Warnings: deliveryWarnings(d)}
}

// deliveryWarnings are the lasting removed-path warnings stored on a row.
func deliveryWarnings(d sqlite.Delivery) []string {
	var out []string
	for _, f := range d.FileResults {
		if f.Warning != "" && (f.State == "present" || f.State == "retained") {
			out = append(out, f.Warning)
		}
	}
	return out
}

// GetRematch reads one operation.
func (s *Service) GetRematch(ctx context.Context, id string) (RematchStatus, error) {
	op, err := s.db.GetRematchOperation(ctx, id)
	if err != nil {
		return RematchStatus{}, err
	}
	out := RematchStatus{RematchOperation: op, Cinema: []RematchCinema{}}
	deliveries, err := s.db.OperationDeliveries(ctx, id)
	if err != nil {
		return out, err
	}
	for _, d := range deliveries {
		if d.IdempotencyKey == "" {
			continue
		}
		out.Cinema = append(out.Cinema, RematchCinemaOf(d))
	}
	return out, nil
}

// rematchDriveEvery is how often open operations are driven on: a restart's
// interrupted move, a finish that could not remove a file, a rollback that
// could not put one back, an operation whose titles were busy.
var rematchDriveEvery = 30 * time.Second

// startRematchWorker drives every open operation until it is terminal: now,
// for what a restart interrupted, then on every tick.
func (s *Service) startRematchWorker(ctx context.Context) {
	s.rematchMu.Lock()
	s.rematchCtx = ctx
	s.rematchMu.Unlock()
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		tick := time.NewTicker(rematchDriveEvery)
		defer tick.Stop()
		for {
			s.driveRematches(ctx)
			s.driveOffloads(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// driveRematches starts every open operation this process is not already
// driving. Titles busy with an import are waited for (and the operation says
// so); the next pass tries again.
func (s *Service) driveRematches(ctx context.Context) {
	ops, err := s.db.OpenRematchOperations(ctx)
	if err != nil {
		s.log.Warn("re-match operations unreadable; retried on the next pass", "err", err)
		return
	}
	for _, op := range ops {
		if ctx.Err() != nil {
			return
		}
		if s.rematchRunning(op.ID) {
			continue
		}
		workCtx, releaseWork, err := ports.TryWork(ctx, s.admission, "rematch")
		if err != nil {
			return // paused for a drain: resumed after it
		}
		leaseCtx, release, err := s.lockImportTargets(workCtx, 2*time.Second, op.SourceItemID, op.TargetItemID)
		if err != nil {
			releaseWork()
			// A move just started is still inside StartRematch's own lease
			// wait; saying "waiting" now would be noise. runRematch clears
			// the line when the move first runs.
			if op.State == "planned" && time.Since(time.UnixMilli(op.CreatedAt)) < rematchLeaseWait {
				continue
			}
			_ = s.db.SetRematchError(ctx, op.ID, "waiting for its titles: "+err.Error())
			continue
		}
		s.launchRematch(leaseCtx, op.ID, func() { release(); releaseWork() })
	}
}

// runRematch drives one operation from wherever it stands; the caller holds
// both titles. An error leaves the operation where it is, for the next pass.
func (s *Service) runRematch(ctx context.Context, id string) error {
	bg := context.WithoutCancel(ctx)
	op, err := s.db.GetRematchOperation(bg, id)
	if err != nil {
		return fmt.Errorf("read operation: %w", err)
	}
	var stored rematchStored
	if err := json.Unmarshal(op.Plan, &stored); err != nil {
		return s.db.SetRematchState(bg, id, "failed", "unreadable plan: "+err.Error(), nil)
	}
	moves, err := s.db.ListRematchMoves(bg, id)
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	switch op.State {
	case "cancel_pending":
		return s.rollbackRematch(bg, op, moves, "", "")
	case "committed":
		return s.finishRematch(bg, op, stored, moves)
	case "complete", "cancelled", "failed":
		return nil
	}
	if err := s.db.SetRematchState(bg, id, "moving", "", nil); err != nil {
		return err
	}
	progress := &rematchProgress{s: s, id: id, done: op.BytesDone}
	for i := range moves {
		mv := &moves[i]
		if mv.State != "rematch_planned" {
			continue
		}
		err := s.moveRematchFile(ctx, mv, progress)
		if err == nil {
			mv.State = "rematch_moved"
			if err := s.db.PutRematchMove(bg, *mv); err != nil {
				return fmt.Errorf("journal %s: %w", mv.Source, err)
			}
			continue
		}
		var review *rematchReviewError
		if errors.As(err, &review) {
			// Resume never moves a file whose source changed: that file is
			// failed for review, the rest go on.
			mv.State, mv.Error = "rematch_failed", review.msg
			if err := s.db.PutRematchMove(bg, *mv); err != nil {
				return fmt.Errorf("journal %s: %w", mv.Source, err)
			}
			s.log.Warn("re-match file needs review", "operation", id, "file", mv.Source, "reason", review.msg)
			continue
		}
		current, readErr := s.db.GetRematchOperation(bg, id)
		if readErr != nil {
			return fmt.Errorf("read operation: %w", readErr)
		}
		if ctx.Err() != nil && current.State != "cancel_pending" {
			return nil // shutting down: the journal resumes it
		}
		if current.State == "cancel_pending" {
			return s.rollbackRematch(bg, current, moves, "cancelled", "")
		}
		return s.rollbackRematch(bg, current, moves, "failed", err.Error())
	}
	_ = s.db.SetRematchProgress(bg, id, progress.done)
	current, err := s.db.GetRematchOperation(bg, id)
	if err != nil {
		return fmt.Errorf("read operation: %w", err)
	}
	if current.State == "cancel_pending" {
		return s.rollbackRematch(bg, current, moves, "cancelled", "")
	}
	if err := s.db.SetRematchState(bg, id, "moved", "", nil); err != nil {
		return err
	}
	if err := s.commitRematch(bg, op, stored, moves); err != nil {
		s.log.Error("re-match commit failed; rolling back", "operation", id, "err", err)
		current, readErr := s.db.GetRematchOperation(bg, id)
		if readErr != nil {
			return fmt.Errorf("read operation: %w", readErr)
		}
		if current.State == "cancel_pending" {
			return s.rollbackRematch(bg, current, moves, "cancelled", "")
		}
		if errors.Is(err, sqlite.ErrRematchStale) {
			// The library changed under the move (a scan, an import): say
			// so, rather than a bare "failed".
			return s.rollbackRematch(bg, current, moves, "failed", "a library scan or import changed this title while the files moved ("+err.Error()+"); every file is back where it was; review and run it again")
		}
		return s.rollbackRematch(bg, current, moves, "failed", err.Error())
	}
	if op, err = s.db.GetRematchOperation(bg, id); err != nil {
		return fmt.Errorf("read operation: %w", err)
	}
	if moves, err = s.db.ListRematchMoves(bg, id); err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	return s.finishRematch(bg, op, stored, moves)
}

type rematchReviewError struct{ msg string }

func (e *rematchReviewError) Error() string { return e.msg }

type rematchProgress struct {
	s    *Service
	id   string
	done int64
	last time.Time
}

func (p *rematchProgress) add(n int64) {
	p.done += n
	if time.Since(p.last) > time.Second {
		p.last = time.Now()
		_ = p.s.db.SetRematchProgress(context.Background(), p.id, p.done)
	}
}

// moveRematchFile puts one file at its target, by what it observes: source
// present and target absent, move; target present and ours, done; anything
// else is a file to review, never overwritten.
func (s *Service) moveRematchFile(ctx context.Context, mv *sqlite.RematchMove, progress *rematchProgress) error {
	if mv.Mode == "relink" {
		return nil
	}
	srcInfo, srcErr := os.Lstat(mv.Source)
	dstInfo, dstErr := os.Lstat(mv.Target)
	srcSame := srcErr == nil && fileIdentityString(srcInfo) == mv.Identity
	switch {
	case dstErr == nil && srcErr != nil && mv.Mode == "rename":
		if inodeOf(fileIdentityString(dstInfo)) != inodeOf(mv.Identity) {
			return &rematchReviewError{"the source is gone and another file stands at the destination"}
		}
	case dstErr == nil && srcSame && mv.Mode == "copy":
		got, _, err := fileDigest(ctx, mv.Target)
		if err != nil {
			return err
		}
		want := mv.SHA256
		if want == "" {
			if want, _, err = fileDigest(ctx, mv.Source); err != nil {
				return err
			}
		}
		if got != want {
			return &rematchReviewError{"another file already stands at " + mv.Target}
		}
		mv.SHA256 = got
	case dstErr == nil:
		return &rematchReviewError{"another file already stands at " + mv.Target}
	case srcErr != nil:
		return &rematchReviewError{"the file is no longer at " + mv.Source}
	case !srcSame:
		return &rematchReviewError{"the file at " + mv.Source + " changed since the preview"}
	default:
		if err := os.MkdirAll(filepath.Dir(mv.Target), 0o755); err != nil {
			return err
		}
		if mv.Mode == "rename" {
			if err := os.Rename(mv.Source, mv.Target); err != nil {
				return err
			}
			if err := syncPublicationDirectory(mv.Target); err != nil {
				return err
			}
		} else {
			sum, err := copyVerified(ctx, mv.Source, mv.Temporary, mv.Target, progress.add)
			if err != nil {
				return err
			}
			mv.SHA256 = sum
		}
	}
	for _, sc := range mv.Sidecars {
		if err := moveSidecar(ctx, sc[0], sc[1], mv.Mode, mv.OperationID); err != nil {
			s.log.Warn("re-match sidecar not moved", "from", sc[0], "err", err)
		}
	}
	return nil
}

// copyVerified copies src to temp beside dest, fsyncs it, verifies size and
// SHA-256 against the source, and publishes it by rename. A cancelled ctx
// stops the copy and removes temp; the source is never touched.
func copyVerified(ctx context.Context, src, temp, dest string, progress func(int64)) (string, error) {
	_ = os.Remove(temp)
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		_ = out.Close()
		_ = os.Remove(temp)
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 4<<20)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return fail(err)
			}
			h.Write(buf[:n])
			written += int64(n)
			if progress != nil {
				progress(int64(n))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(rerr)
		}
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	want := hex.EncodeToString(h.Sum(nil))
	got, size, err := fileDigest(ctx, temp)
	if err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	if info, e := os.Stat(src); e != nil || got != want || size != written || info.Size() != written {
		_ = os.Remove(temp)
		return "", fmt.Errorf("the copy of %s did not verify against the source", src)
	}
	if _, err := os.Lstat(dest); err == nil {
		_ = os.Remove(temp)
		return "", &rematchReviewError{"another file already stands at " + dest}
	}
	if err := os.Rename(temp, dest); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	if err := syncPublicationDirectory(dest); err != nil {
		return "", err
	}
	return want, nil
}

// moveSidecar moves one sidecar the same way its video went; already moved
// or missing is fine. A copy goes through a hidden temp name beside it, which
// a restart or rollback removes.
func moveSidecar(ctx context.Context, from, to, mode, opID string) error {
	if _, err := os.Lstat(from); err != nil {
		return nil
	}
	if _, err := os.Lstat(to); err == nil {
		return nil
	}
	if mode == "rename" {
		return os.Rename(from, to)
	}
	_, err := copyVerified(ctx, from, sidecarTemp(to, opID), to, nil)
	return err
}

func sidecarTemp(to, opID string) string {
	return filepath.Join(filepath.Dir(to), "."+filepath.Base(to)+".rematch-"+opID+".partial")
}

// rematchRemove is os.Remove; tests stand in a failing one.
var rematchRemove = os.Remove

// rollbackRematch puts every moved file back (a rename reversed, a copy's
// destination removed) and ends the operation cancelled or failed. A file it
// cannot put back keeps the operation cancel_pending, saying what is where,
// and the next pass tries again. state "" reads the end it was rolling back
// to from the operation's result.
func (s *Service) rollbackRematch(ctx context.Context, op sqlite.RematchOperation, moves []sqlite.RematchMove, state, reason string) error {
	var pending struct {
		Rollback string `json:"rollback"`
		Reason   string `json:"reason"`
	}
	_ = json.Unmarshal(op.Result, &pending)
	if state == "" {
		state, reason = pending.Rollback, pending.Reason
		if state == "" {
			state = "cancelled"
		}
	}
	var problems []string
	for i := len(moves) - 1; i >= 0; i-- {
		mv := &moves[i]
		if mv.Temporary != "" {
			_ = os.Remove(mv.Temporary)
		}
		for _, sc := range mv.Sidecars {
			_ = os.Remove(sidecarTemp(sc[1], mv.OperationID))
		}
		if mv.State != "rematch_moved" && mv.State != "rematch_planned" {
			continue
		}
		if mv.Mode != "relink" {
			if err := s.putBack(ctx, mv); err != nil {
				problems = append(problems, err.Error())
				continue
			}
			_ = os.Remove(filepath.Dir(mv.Target)) // only if empty
		}
		mv.State = "rematch_rolled_back"
		if err := s.db.PutRematchMove(ctx, *mv); err != nil {
			problems = append(problems, fmt.Sprintf("journal %s: %v", mv.Source, err))
		}
	}
	if len(problems) > 0 {
		raw, _ := json.Marshal(map[string]string{"rollback": state, "reason": reason})
		_ = s.db.SetRematchState(ctx, op.ID, "cancel_pending", "", raw)
		msg := "putting files back is not finished; retrying: " + strings.Join(problems, "; ")
		_ = s.db.SetRematchError(ctx, op.ID, msg)
		return errors.New(msg)
	}
	if err := s.db.SetRematchState(ctx, op.ID, state, reason, nil); err != nil {
		return err
	}
	_ = s.db.PruneRematchMoves(ctx, op.ID)
	s.log.Info("re-match ended without moving", "operation", op.ID, "state", state, "reason", reason)
	return nil
}

// putBack undoes one move that has not committed: a rename is reversed, a
// copy's destination removed once it is known to be ours (by its recorded
// digest, or — published before the digest reached the journal — by
// matching the source's bytes). The source is never touched.
func (s *Service) putBack(ctx context.Context, mv *sqlite.RematchMove) error {
	for _, sc := range mv.Sidecars {
		if mv.Mode == "rename" {
			if _, err := os.Lstat(sc[0]); err != nil {
				if _, e := os.Lstat(sc[1]); e == nil {
					if err := os.Rename(sc[1], sc[0]); err != nil {
						return fmt.Errorf("%s is still at %s: %v", sc[0], sc[1], err)
					}
				}
			}
		} else if _, err := os.Lstat(sc[0]); err == nil {
			if err := rematchRemove(sc[1]); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("the copy %s could not be removed: %v", sc[1], err)
			}
		}
	}
	info, err := os.Lstat(mv.Target)
	if err != nil {
		return nil // nothing was placed
	}
	switch mv.Mode {
	case "rename":
		if inodeOf(fileIdentityString(info)) != inodeOf(mv.Identity) {
			return nil // not ours
		}
		if _, e := os.Lstat(mv.Source); e == nil {
			return fmt.Errorf("%s and %s both exist; review which to keep", mv.Source, mv.Target)
		}
		if err := os.Rename(mv.Target, mv.Source); err != nil {
			return fmt.Errorf("%s is still at %s: %v", mv.Source, mv.Target, err)
		}
	case "copy":
		if _, e := os.Lstat(mv.Source); e != nil {
			return fmt.Errorf("the original %s is gone; the copy stays at %s", mv.Source, mv.Target)
		}
		ours := false
		got, _, derr := fileDigest(ctx, mv.Target)
		switch {
		case derr != nil:
			return fmt.Errorf("the copy %s cannot be read: %v", mv.Target, derr)
		case mv.SHA256 != "":
			ours = got == mv.SHA256
		default:
			want, _, serr := fileDigest(ctx, mv.Source)
			if serr != nil {
				return fmt.Errorf("the original %s cannot be read: %v", mv.Source, serr)
			}
			ours = got == want
		}
		if !ours {
			return nil
		}
		if err := rematchRemove(mv.Target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("the copy %s could not be removed: %v", mv.Target, err)
		}
	}
	return nil
}

// rematchResult is the operation's record of what happened.
type rematchResult struct {
	Moved         []rematchMoved   `json:"moved"`
	LeftOut       []string         `json:"leftOut"`
	Failed        []RematchPair    `json:"failed"`
	Blocklisted   []RematchRelease `json:"blocklisted"`
	Replaced      []string         `json:"replaced,omitempty"`
	SourceRemoved bool             `json:"sourceRemoved"`
	LeftBehind    []string         `json:"leftBehind,omitempty"`
	CinemaNote    string           `json:"cinemaNote,omitempty"`
}

type rematchMoved struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Episodes []int  `json:"episodes,omitempty"`
	Mode     string `json:"mode"`
}

func (s *Service) commitRematch(ctx context.Context, op sqlite.RematchOperation, stored rematchStored, moves []sqlite.RematchMove) error {
	p := stored.Preview
	source, err := s.db.GetMediaItemFull(ctx, op.SourceItemID)
	if err != nil {
		return err
	}
	target, err := s.db.GetMediaItemFull(ctx, op.TargetItemID)
	if err != nil {
		return err
	}
	lines := map[int64]RematchMovePlan{}
	for _, m := range p.Moves {
		lines[m.FileID] = m
	}
	files := map[int64]domain.MediaFile{}
	for _, f := range source.Files {
		files[f.ID] = f
	}
	commit := sqlite.RematchCommit{OperationID: op.ID, SourceItemID: source.ID, TargetItemID: target.ID, TargetPath: stored.TargetPath, RemoveSource: stored.RemoveSource}
	result := rematchResult{Moved: []rematchMoved{}, LeftOut: []string{}, Failed: []RematchPair{}, Blocklisted: []RematchRelease{}, SourceRemoved: stored.RemoveSource, CinemaNote: p.Cinema}
	var placed []rematchPlaced
	var strays []string
	// Blocklisting is for an item that stays: decided from the commit's own
	// outcome (a failed move keeps the source), once every move is read.
	var blocks []sqlite.RematchBlocklist
	var blocked []RematchRelease
	for _, mv := range moves {
		line := lines[mv.FileID]
		if mv.State == "rematch_failed" {
			result.Failed = append(result.Failed, RematchPair{From: mv.Source, To: mv.Error})
			commit.RemoveSource = false
			continue
		}
		if mv.State != "rematch_moved" {
			continue
		}
		f := files[mv.FileID]
		fc := sqlite.RematchFileCommit{FileID: mv.FileID, OldPath: mv.Source, NewPath: mv.Target, TargetItemID: target.ID, CopyID: p.TargetCopyID, EpisodeIDs: stored.EpisodeIDs[mv.FileID], Sidecars: mv.Sidecars}
		if target.Kind != domain.KindSeries {
			fc.EpisodeIDs = nil
		}
		commit.Files = append(commit.Files, fc)
		result.Moved = append(result.Moved, rematchMoved{From: mv.Source, To: mv.Target, Episodes: line.Episodes, Mode: mv.Mode})
		// A move's removed paths travel with its expected file: Cinema's
		// expected_not_placed guard is per request.
		var gone []string
		if mv.Source != mv.Target {
			gone = append(gone, mv.Source)
		}
		for _, r := range line.Replaces {
			gone = append(gone, r.Path)
			result.Replaced = append(result.Replaced, r.Path)
		}
		if info, err := os.Stat(mv.Target); err == nil {
			placed = append(placed, rematchPlaced{File: ports.ExpectedFile{Path: mv.Target, Bytes: info.Size(), MtimeNs: info.ModTime().UnixNano()}, Removed: gone})
		} else {
			strays = append(strays, gone...)
		}
		if p.BlocklistOn && f.SourceGUID != "" && f.SourceIndexerID != 0 {
			for _, r := range p.Blocklist {
				if r.FileID != f.ID {
					continue
				}
				scope, eps := "download", []int64(nil)
				if source.Kind == domain.KindSeries {
					scope, eps = "episodes", f.EpisodeIDs
				}
				blocks = append(blocks, sqlite.RematchBlocklist{
					OperationID: op.ID + "/blocklist/" + strconv.FormatInt(f.ID, 10), MediaItemID: source.ID, CopyID: f.CopyID,
					Scope: scope, EpisodeIDs: eps, SourceGeneration: f.SourceGeneration, SourceTransfer: f.SourceTransfer,
					SourceRelease: f.SourceRelease, SourceIndexer: f.SourceIndexer, SourceGUID: f.SourceGUID, SourceIndexerID: f.SourceIndexerID,
					Reason: "re-matched to " + target.Title})
				blocked = append(blocked, r)
			}
		}
	}
	for _, l := range p.LeftOut {
		if !l.Leave {
			continue // never reached: execution refuses unresolved lines
		}
		commit.LeftOut = append(commit.LeftOut, l.FileID)
		result.LeftOut = append(result.LeftOut, l.From)
	}
	if !commit.RemoveSource {
		commit.Blocklist, result.Blocklisted = blocks, append(result.Blocklisted, blocked...)
	}
	result.SourceRemoved = commit.RemoveSource
	data := map[string]any{"failed": result.Failed, "operation": op.ID, "from": source.ID, "fromTitle": source.Title, "to": target.ID, "toTitle": target.Title,
		"moved": result.Moved, "blocklisted": result.Blocklisted, "leftOut": result.LeftOut, "sourceRemoved": commit.RemoveSource}
	commit.History = append(commit.History, sqlite.HistoryWrite{Type: HistoryRematched, MediaItemID: target.ID, Data: data})
	if source.ID != target.ID {
		commit.History = append(commit.History, sqlite.HistoryWrite{Type: HistoryRematched, MediaItemID: source.ID, Data: data})
	}
	if len(placed) > 0 || len(strays) > 0 {
		if commit.Outbox, err = s.rematchOutbox(ctx, op.ID, source, target, placed, strays); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(result)
	commit.Result = raw
	if err := s.db.CommitRematch(ctx, commit); err != nil {
		return err
	}
	s.InvalidateWanted()
	return nil
}

// HistoryRematched records a Re-match on both items: from, to and every
// path that moved.
const HistoryRematched = "rematched"

// rematchSourceChanged marks a committed copy whose source was not the file
// that was copied, so it was left alone.
const rematchSourceChanged = "the original changed since the copy and was left in place"

// finishRematch removes what a copy left at the source, removes what a move
// replaces (import's removeExistingFiles), and an emptied adopted item's
// folders. The operation completes only when every move is done; anything
// left undone stays on the operation as its reason, and the next pass tries
// again.
func (s *Service) finishRematch(ctx context.Context, op sqlite.RematchOperation, stored rematchStored, moves []sqlite.RematchMove) error {
	// Every file this operation placed is kept by a replacement, so two
	// moved files into one title never remove each other.
	var keep []string
	for _, mv := range moves {
		if mv.State == "rematch_committed" || mv.State == "rematch_done" {
			keep = append(keep, mv.Target)
		}
	}
	var target domain.MediaItem
	targetRead := false
	var problems []string
	for i := range moves {
		mv := &moves[i]
		if mv.State != "rematch_committed" {
			continue
		}
		if mv.Mode == "copy" {
			if info, err := os.Lstat(mv.Source); err == nil && fileIdentityString(info) == mv.Identity {
				if err := rematchRemove(mv.Source); err != nil && !os.IsNotExist(err) {
					problems = append(problems, fmt.Sprintf("the original %s could not be removed (the library copy is at %s): %v", mv.Source, mv.Target, err))
					continue
				}
			} else if err == nil {
				// Something else stands at the source now (changed since the
				// copy): never removed, and reported rather than called done.
				mv.Error = rematchSourceChanged
			}
			sidecarsLeft := false
			for _, sc := range mv.Sidecars {
				if _, err := os.Lstat(sc[1]); err == nil {
					if err := rematchRemove(sc[0]); err != nil && !os.IsNotExist(err) {
						problems = append(problems, fmt.Sprintf("the original %s could not be removed: %v", sc[0], err))
						sidecarsLeft = true
					}
				}
			}
			if sidecarsLeft {
				continue
			}
		}
		if mv.Replace {
			// The moved file is committed; what it replaces leaves now, by
			// import's own path (bytes, row, placed subtitles). Journaled:
			// it is repeated until every replaced file is gone.
			if !targetRead {
				t, err := s.db.GetMediaItemFull(ctx, op.TargetItemID)
				if err != nil {
					problems = append(problems, "read the target: "+err.Error())
					continue
				}
				target, targetRead = t, true
			}
			var eps []int64
			if target.Kind == domain.KindSeries {
				eps = append([]int64{}, mv.EpisodeIDs...)
			}
			s.removeExistingFiles(ctx, target, mv.CopyID, eps, keep...)
			if left := s.replacedLeft(ctx, mv.ReplaceIDs); len(left) > 0 {
				problems = append(problems, "the replaced file(s) "+strings.Join(left, ", ")+" could not be removed")
				continue
			}
		}
		mv.State = "rematch_done"
		if err := s.db.PutRematchMove(ctx, *mv); err != nil {
			problems = append(problems, fmt.Sprintf("journal %s: %v", mv.Source, err))
		}
	}
	if len(problems) > 0 {
		msg := "finishing: " + strings.Join(problems, "; ") + "; retrying"
		_ = s.db.SetRematchError(ctx, op.ID, msg)
		return errors.New(msg)
	}
	var result rematchResult
	_ = json.Unmarshal(op.Result, &result)
	if result.SourceRemoved && stored.Preview.Source.Folder != "" {
		result.LeftBehind = removeEmptyDirs(stored.Preview.Source.Folder)
	}
	// A copy's source that changed after the copy is never removed; the
	// journal remembers it across passes.
	for _, mv := range moves {
		if mv.Error == rematchSourceChanged && !slices.Contains(result.LeftBehind, mv.Source) {
			result.LeftBehind = append(result.LeftBehind, mv.Source)
		}
	}
	raw, _ := json.Marshal(result)
	if err := s.db.SetRematchState(ctx, op.ID, "complete", "", raw); err != nil {
		return err
	}
	_ = s.db.PruneRematchMoves(ctx, op.ID)
	s.log.Info("re-match complete", "operation", op.ID, "moved", len(result.Moved), "left out", len(result.LeftOut))
	return nil
}

// replacedLeft lists the replaced files still in the library.
func (s *Service) replacedLeft(ctx context.Context, ids []int64) []string {
	var left []string
	for _, id := range ids {
		var path string
		if err := s.db.R.QueryRowContext(ctx, `SELECT path FROM media_files WHERE id=?`, id).Scan(&path); err == nil {
			left = append(left, path)
		}
	}
	return left
}

// removeEmptyDirs removes dir and its empty subfolders, bottom up, and lists
// whatever is left in it (never removed).
func removeEmptyDirs(dir string) []string {
	var dirs []string
	var left []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		} else {
			left = append(left, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	return left
}

// rematchPlaced is one file a move placed and the paths that left because of
// it (its source, what it replaced).
type rematchPlaced struct {
	File    ports.ExpectedFile
	Removed []string
}

// rematchPair is the unit of a Cinema request: an expected file with its
// removed paths (exp nil: removed paths whose file could not be probed).
type rematchPair struct {
	exp *ports.ExpectedFile
	rem []string
}

// rematchOutbox writes the move's notifications (ADR 0027 item 9). Cinema
// gets one sub-delivery per library root touched, keyed by operation, whose
// path is the common parent of the files placed and the files that left;
// every other notifier gets one moved message.
func (s *Service) rematchOutbox(ctx context.Context, opID string, source, target domain.MediaItem, placed []rematchPlaced, strays []string) ([]sqlite.OutboxWrite, error) {
	notifiers, err := s.db.ListNotifiers(ctx)
	if err != nil {
		return nil, err
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	var paths, removed []string
	for _, f := range placed {
		paths = append(paths, f.File.Path)
		removed = append(removed, f.Removed...)
	}
	removed = append(removed, strays...)
	whole := ports.Notification{Event: "moved", Title: "Re-matched: " + target.Title,
		Body:   fmt.Sprintf("%d file%s moved from %s to %s", len(placed), pluralS(len(placed)), source.Title, target.Title),
		Fields: map[string]string{"from": source.Title, "to": target.Title},
		Import: &ports.ImportInfo{MediaItemID: target.ID, FromItemID: source.ID, Paths: paths, Dirs: uniqueDirs(paths), Kind: string(target.Kind),
			Title: target.Title, TmdbID: target.IDs.TMDB, ImdbID: target.IDs.IMDB, RemovedPaths: removed}}
	wholePayload, err := json.Marshal(whole)
	if err != nil {
		return nil, err
	}
	// Group by library root: one request per root (a removed path under
	// another root than its file's is its own pair there).
	groups := map[string][]rematchPair{}
	rootOf := func(path string) string {
		best := ""
		for _, r := range roots {
			if (path == r.Path || strings.HasPrefix(path, strings.TrimSuffix(r.Path, "/")+"/")) && len(r.Path) > len(best) {
				best = r.Path
			}
		}
		return best
	}
	for _, f := range placed {
		f := f
		root := rootOf(f.File.Path)
		pair := rematchPair{exp: &f.File}
		for _, r := range f.Removed {
			if rr := rootOf(r); rr != root {
				groups[rr] = append(groups[rr], rematchPair{rem: []string{r}})
				continue
			}
			pair.rem = append(pair.rem, r)
		}
		groups[root] = append(groups[root], pair)
	}
	for _, r := range strays {
		groups[rootOf(r)] = append(groups[rootOf(r)], rematchPair{rem: []string{r}})
	}
	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []sqlite.OutboxWrite
	for _, cfg := range notifiers {
		if !cfg.Enabled || !cfg.OnImport {
			continue
		}
		if !ports.QueuedNotifier(cfg) {
			out = append(out, sqlite.OutboxWrite{NotifierID: cfg.ID, MediaItemID: target.ID, Event: "moved", Payload: string(wholePayload), OperationID: opID})
			continue
		}
		for _, root := range keys {
			var all []string
			for _, pr := range groups[root] {
				if pr.exp != nil {
					all = append(all, pr.exp.Path)
				}
				all = append(all, pr.rem...)
			}
			dir := commonParent(all)
			if dir == "" || !strings.HasPrefix(dir+"/", strings.TrimSuffix(root, "/")+"/") {
				dir = root
			}
			// Every expected file is probed: a request with removed_paths is
			// probe-only (contract 4.4), so a file left off it would never be
			// confirmed. A file and the paths that left because of it share
			// a request, and every request is one Cinema accepts.
			for chunk, pairs := range chunkPairs(groups[root]) {
				var exp []ports.ExpectedFile
				var part []string
				for _, pr := range pairs {
					if pr.exp != nil {
						exp = append(exp, *pr.exp)
					}
					part = append(part, pr.rem...)
				}
				n := whole
				info := *whole.Import
				info.Dirs, info.Paths, info.RemovedPaths, info.ExpectedFiles = []string{dir}, nil, part, nil
				for _, f := range exp {
					info.Paths = append(info.Paths, f.Path)
				}
				if len(exp) > 0 {
					info.ExpectedFiles = map[string][]ports.ExpectedFile{dir: exp}
				}
				n.Import = &info
				payload, err := json.Marshal(n)
				if err != nil {
					return nil, err
				}
				key := RematchDeliveryKey(opID, dir)
				if chunk > 0 {
					key += "-" + strconv.Itoa(chunk)
				}
				out = append(out, sqlite.OutboxWrite{NotifierID: cfg.ID, MediaItemID: target.ID, Directory: dir, IdempotencyKey: key, ExpectedFiles: exp,
					Event: "moved", Payload: string(payload), OperationID: opID})
			}
		}
	}
	return out, nil
}

// chunkPairs splits pairs into requests Cinema accepts: at most
// ports.MaxExpectedFiles expected files and as many removed paths each, each
// list within ports.MaxExpectedFileBytes as JSON. A pair is never split.
func chunkPairs(pairs []rematchPair) [][]rematchPair {
	var out [][]rematchPair
	var cur []rematchPair
	var exp []ports.ExpectedFile
	var rem []string
	fits := func(e []ports.ExpectedFile, r []string) bool {
		if len(e) > ports.MaxExpectedFiles || len(r) > ports.MaxExpectedFiles {
			return false
		}
		return expectedJSONSize(e) <= ports.MaxExpectedFileBytes && pathsJSONSize(r) <= ports.MaxExpectedFileBytes
	}
	for _, p := range pairs {
		ne := exp
		if p.exp != nil {
			ne = append(append([]ports.ExpectedFile(nil), exp...), *p.exp)
		}
		nr := append(append([]string(nil), rem...), p.rem...)
		if len(cur) > 0 && !fits(ne, nr) {
			out, cur = append(out, cur), nil
			ne, nr = nil, p.rem
			if p.exp != nil {
				ne = []ports.ExpectedFile{*p.exp}
			}
		}
		cur, exp, rem = append(cur, p), ne, nr
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func pathsJSONSize(paths []string) int {
	size := 2
	for _, p := range paths {
		raw, _ := json.Marshal(p)
		size += len(raw) + 1
	}
	return size
}

// RematchDeliveryKey is a move's sub-delivery key: op/<operation-id>/<dir-hash>
// (contract amendment 4.1).
func RematchDeliveryKey(opID, dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "op/" + opID + "/" + hex.EncodeToString(sum[:])[:16]
}

func uniqueDirs(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		d := filepath.Dir(p)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// commonParent is the deepest folder holding every path.
func commonParent(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	parent := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		for parent != "/" && parent != "." && !strings.HasPrefix(p, strings.TrimSuffix(parent, "/")+"/") {
			parent = filepath.Dir(parent)
		}
	}
	return parent
}
