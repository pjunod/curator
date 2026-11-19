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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Offload (ADR 0028): free the space, keep the title.
//
// An Offload deletes every file of a movie, show, season or episode (video,
// subtitles, every quality copy) and keeps the title: identity, profile,
// copies, history, blocklist. The scope is marked offloaded (its own stored
// state, folded into the one Mon computation, so no search path grabs it).
// It is a durable operation like a Re-match (the same recurring worker drives
// it): a preview with no side effects, then one transaction that records the
// scope and the file list, sets the state and writes Cinema's operation-keyed
// sub-deliveries (held until the files are gone), then the deletes, resumable
// after a crash. A share that is not mounted makes the operation wait; an
// unlink that fails is a state with its reason and is retried; Stop ends the
// remaining deletes. Re-acquire is the way back.

// ErrInvalidOffload is a request that cannot be answered as asked.
var ErrInvalidOffload = errors.New("invalid offload")

// ErrOffloadStalePreview is an execution whose preview no longer describes
// the library: preview again.
var ErrOffloadStalePreview = errors.New("the library changed since this preview; preview again")

// OffloadBlockedError means the share cannot be read to confirm a file list.
type OffloadBlockedError struct{ Blocked OffloadBlocked }

func (e *OffloadBlockedError) Error() string { return e.Blocked.Message }

// OffloadBlocked names what holds an Offload back, and that waiting helps.
type OffloadBlocked struct {
	Reason  string `json:"reason"` // download | import | replacement | rematch | offload | unmounted
	Ref     string `json:"ref,omitempty"`
	Message string `json:"message"`
	Retry   bool   `json:"retry"`
}

// OffloadRequest is a preview or an execution. Scope is item (the whole
// movie or show), season (Season) or episode (EpisodeID).
type OffloadRequest struct {
	ItemID    int64  `json:"-"`
	Scope     string `json:"scope"`
	Season    int    `json:"season,omitempty"`
	EpisodeID int64  `json:"episodeId,omitempty"`

	// Execution only.
	RequestID       string `json:"requestId,omitempty"`
	PlanFingerprint string `json:"fingerprint,omitempty"`
}

// OffloadFilePlan is one file the Offload deletes.
type OffloadFilePlan struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // media | sidecar
	// Bytes is the file's size; Shared says another hard link keeps the bytes
	// allocated (a seeding copy), so deleting this one frees nothing yet.
	Bytes    int64    `json:"bytes"`
	Shared   bool     `json:"shared,omitempty"`
	Episodes []string `json:"episodes,omitempty"` // S01E02 labels
}

// OffloadPreview is what an Offload would do, before anything happens.
type OffloadPreview struct {
	Item  RematchItemRef    `json:"item"`
	Scope string            `json:"scope"`
	Label string            `json:"label"` // "Dune (2021)", "Season 2", "S01E03 · Title"
	Files []OffloadFilePlan `json:"files"`
	// Bytes is every byte deleted; Freed is what returns to the volume now
	// (hard-linked bytes are not counted until the other link goes).
	Bytes int64 `json:"bytes"`
	Freed int64 `json:"freed"`
	// FreeBefore and FreeAfter are the library volume's free space (-1 =
	// unknown) before and after, by Freed.
	FreeBefore int64  `json:"freeBefore"`
	FreeAfter  int64  `json:"freeAfter"`
	Root       string `json:"root,omitempty"`
	// Episodes lists what becomes offloaded, in order; Widened is the part
	// the request did not name: a file covering several episodes goes whole.
	Episodes []string `json:"episodes,omitempty"`
	Widened  []string `json:"widened,omitempty"`
	// Cinema says what Cinema will do; CinemaWarning is true when it will
	// drop the title and its watch history (it does not advertise offload).
	// Advisory: never a gate.
	Cinema        string          `json:"cinema,omitempty"`
	CinemaWarning bool            `json:"cinemaWarning,omitempty"`
	Blocked       *OffloadBlocked `json:"blocked,omitempty"`
	Fingerprint   string          `json:"fingerprint"`
	Summary       string          `json:"summary"`
}

// offloadPlan is a preview plus what execution needs that the page does not.
type offloadPlan struct {
	revision         sqlite.OffloadRevision
	preview          OffloadPreview
	item             domain.MediaItem
	movie            bool
	whole            bool // item scope of a series: the stored whole-show intent
	seasons          []int
	episodes         []int64 // every episode that becomes offloaded
	fileless         []int64 // those of them with no file at plan time
	fileEps          map[string][]int64
	fileCopies       map[string]int64
	fileGenerations  map[string]string
	files            []sqlite.OffloadFile
	identities       map[string]string
	rootIdentities   map[string]string
	parentIdentities map[string]string
	roots            []string
}

// offloadStored is the plan an operation executes (offload_operations.plan).
type offloadStored struct {
	Request  OffloadRequest `json:"request"`
	Label    string         `json:"label"`
	Title    string         `json:"title"`
	Year     int            `json:"year,omitempty"`
	Kind     string         `json:"kind"`
	Movie    bool           `json:"movie,omitempty"`
	Seasons  []int          `json:"seasons,omitempty"`
	Episodes []int64        `json:"episodes,omitempty"`
	// Fileless: episodes of the scope that had no file when it was planned.
	Fileless []int64 `json:"fileless,omitempty"`
	// FileEpisodes maps a media file to the episodes it covered, so a partial
	// Re-acquire can narrow the operation's Cinema requests to what is still gone.
	FileCopies       map[string]int64   `json:"fileCopies,omitempty"`
	FileEpisodes     map[string][]int64 `json:"fileEpisodes,omitempty"`
	Labels           []string           `json:"episodeLabels,omitempty"`
	Identities       map[string]string  `json:"fileIdentities,omitempty"`
	FileGenerations  map[string]string  `json:"fileGenerations,omitempty"`
	RootIdentities   map[string]string  `json:"rootIdentities,omitempty"`
	ParentIdentities map[string]string  `json:"parentIdentities,omitempty"`
	Roots            []string           `json:"roots,omitempty"`
	Folder           string             `json:"folder,omitempty"`
}

const (
	offloadParallelism       = 4
	offloadVolumeParallelism = 2
)

func episodeLabel(e domain.Episode) string {
	return fmt.Sprintf("S%02dE%02d", e.SeasonNumber, e.EpisodeNumber)
}

// PreviewOffload answers what an Offload would do, without side effects.
func (s *Service) PreviewOffload(ctx context.Context, req OffloadRequest) (OffloadPreview, error) {
	plan, err := s.planOffload(ctx, req)
	if err != nil {
		return OffloadPreview{}, err
	}
	return plan.preview, nil
}

func (s *Service) planOffload(ctx context.Context, req OffloadRequest) (*offloadPlan, error) {
	revision, err := s.db.OffloadRevision(ctx, req.ItemID)
	if err != nil {
		return nil, err
	}
	item, err := s.db.GetMediaItemFull(ctx, req.ItemID)
	if err != nil {
		return nil, err
	}
	if item.Kind != domain.KindMovie && item.Kind != domain.KindSeries {
		return nil, fmt.Errorf("%w: only a movie or a show can be offloaded", ErrInvalidOffload)
	}
	plan := &offloadPlan{item: item, revision: revision}
	p := &plan.preview
	p.Item = RematchItemRef{ID: item.ID, Kind: string(item.Kind), Title: item.Title, Year: item.Year, InLibrary: true, Folder: item.Path}
	p.Scope, p.Files, p.Episodes, p.Widened = req.Scope, []OffloadFilePlan{}, []string{}, []string{}
	title := titleWithYear(item)

	var files []domain.MediaFile
	byEpisode := map[int64]domain.Episode{}
	bySeason := map[int64]int{} // episode → season number
	for _, sn := range item.Seasons {
		for _, e := range sn.Episodes {
			byEpisode[e.ID] = e
			bySeason[e.ID] = sn.Number
		}
	}
	switch item.Kind {
	case domain.KindMovie:
		if req.Scope != "item" && req.Scope != "" {
			return nil, fmt.Errorf("%w: a movie is offloaded whole", ErrInvalidOffload)
		}
		p.Scope, p.Label = "item", title
		if item.Offloaded() {
			return nil, fmt.Errorf("%w: %s is already offloaded", ErrInvalidOffload, item.Title)
		}
		plan.movie = true
		files = item.Files
	case domain.KindSeries:
		requested := map[int64]bool{}
		switch req.Scope {
		case "item", "":
			p.Scope, p.Label = "item", title
			plan.whole = true
			for _, sn := range item.Seasons {
				plan.seasons = append(plan.seasons, sn.Number)
				for _, e := range sn.Episodes {
					requested[e.ID] = true
				}
			}
		case "season":
			found := false
			for _, sn := range item.Seasons {
				if sn.Number != req.Season {
					continue
				}
				found = true
				plan.seasons = append(plan.seasons, sn.Number)
				for _, e := range sn.Episodes {
					requested[e.ID] = true
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: %s has no season %d", ErrInvalidOffload, item.Title, req.Season)
			}
			p.Label = fmt.Sprintf("%s · %s", title, seasonLabel(req.Season))
		case "episode":
			e, ok := byEpisode[req.EpisodeID]
			if !ok {
				return nil, fmt.Errorf("%w: %s has no such episode", ErrInvalidOffload, item.Title)
			}
			requested[e.ID] = true
			p.Label = fmt.Sprintf("%s · %s", title, episodeLabel(e))
			if e.Title != "" {
				p.Label += " · " + e.Title
			}
		default:
			return nil, fmt.Errorf("%w: scope must be item, season or episode", ErrInvalidOffload)
		}
		// Skip what is offloaded already; a season-level scope drops a
		// season flag that is set.
		set := map[int64]bool{}
		for id := range requested {
			if !byEpisode[id].Offloaded() {
				set[id] = true
			}
		}
		var seasons []int
		for _, n := range plan.seasons {
			already := true
			for _, sn := range item.Seasons {
				if sn.Number == n && !sn.Offloaded() {
					already = false
				}
			}
			if !already {
				seasons = append(seasons, n)
			}
		}
		plan.seasons = seasons
		if len(set) == 0 && len(plan.seasons) == 0 {
			return nil, fmt.Errorf("%w: that is already offloaded", ErrInvalidOffload)
		}
		// A file covers its episodes whole: every episode of a multi-episode
		// file goes with it, and every other copy's file of those episodes.
		inScope := func(f domain.MediaFile) bool {
			if req.Scope == "item" || req.Scope == "" {
				return true
			}
			for _, id := range f.EpisodeIDs {
				if set[id] {
					return true
				}
			}
			return false
		}
		chosen := map[int64]bool{}
		for changed := true; changed; {
			changed = false
			for _, f := range item.Files {
				if chosen[f.ID] || !inScope(f) {
					continue
				}
				chosen[f.ID] = true
				changed = true
				files = append(files, f)
				for _, id := range f.EpisodeIDs {
					if !set[id] && !byEpisode[id].Offloaded() {
						set[id] = true
					}
				}
			}
		}
		for id := range set {
			plan.episodes = append(plan.episodes, id)
			if !requested[id] {
				p.Widened = append(p.Widened, episodeLabel(byEpisode[id]))
			}
		}
		sort.Slice(plan.episodes, func(i, j int) bool {
			a, b := byEpisode[plan.episodes[i]], byEpisode[plan.episodes[j]]
			if a.SeasonNumber != b.SeasonNumber {
				return a.SeasonNumber < b.SeasonNumber
			}
			return a.EpisodeNumber < b.EpisodeNumber
		})
		sort.Strings(p.Widened)
		covered := map[int64]bool{}
		for _, f := range files {
			for _, id := range f.EpisodeIDs {
				covered[id] = true
			}
		}
		for _, id := range plan.episodes {
			if !covered[id] {
				plan.fileless = append(plan.fileless, id)
			}
		}
		for _, id := range plan.episodes {
			p.Episodes = append(p.Episodes, episodeLabel(byEpisode[id]))
		}
	}

	// The file list: library files, then the sidecars and untracked
	// subtitles that share their stem.
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	listed := map[string]bool{}
	inLibrary := map[string]bool{}
	for _, f := range item.Files {
		inLibrary[f.Path] = true
	}
	for _, f := range files {
		listed[f.Path] = true
		if plan.fileCopies == nil {
			plan.fileCopies = map[string]int64{}
			plan.fileGenerations = map[string]string{}
		}
		plan.fileCopies[f.Path] = f.CopyID
		plan.fileGenerations[f.Path] = f.FileGeneration
		fp := OffloadFilePlan{Path: f.Path, Kind: "media", Bytes: f.Size, Shared: offloadLinkCount(f.Path) > 1}
		for _, id := range f.EpisodeIDs {
			fp.Episodes = append(fp.Episodes, episodeLabel(byEpisode[id]))
		}
		sort.Strings(fp.Episodes)
		p.Files = append(p.Files, fp)
		plan.files = append(plan.files, sqlite.OffloadFile{Path: f.Path, Kind: "media", FileID: f.ID, Size: f.Size})
		if len(f.EpisodeIDs) > 0 {
			if plan.fileEps == nil {
				plan.fileEps = map[string][]int64{}
			}
			plan.fileEps[f.Path] = append([]int64(nil), f.EpisodeIDs...)
		}
	}
	for _, f := range files {
		for _, sc := range s.offloadSidecars(ctx, f.Path, inLibrary) {
			if listed[sc.path] {
				continue
			}
			listed[sc.path] = true
			p.Files = append(p.Files, OffloadFilePlan{Path: sc.path, Kind: "sidecar", Bytes: sc.size, Shared: offloadLinkCount(sc.path) > 1})
			plan.files = append(plan.files, sqlite.OffloadFile{Path: sc.path, Kind: "sidecar", Size: sc.size})
		}
	}
	for _, f := range p.Files {
		p.Bytes += f.Bytes
		if !f.Shared {
			p.Freed += f.Bytes
		}
	}

	// Where the space comes back.
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range plan.files {
		r := offloadRootOf(roots, f.Path)
		if r == "" {
			// Outside every library root: the folder that holds the title's
			// folder, which keeps existing after the delete.
			if r = item.Path; r != "" {
				r = filepath.Dir(r)
			} else {
				r = filepath.Dir(f.Path)
			}
		}
		if !seen[r] {
			seen[r] = true
			plan.roots = append(plan.roots, r)
		}
	}
	sort.Strings(plan.roots)
	p.FreeBefore, p.FreeAfter = -1, -1
	probe := item.Path
	if len(plan.roots) > 0 {
		p.Root, probe = plan.roots[0], plan.roots[0]
	} else if probe != "" {
		p.Root = probe
	}
	if probe != "" {
		if before := offloadFreeBytes(probe); before >= 0 {
			p.FreeBefore, p.FreeAfter = before, before+p.Freed
		}
	}

	// Blockers: work in flight on the title, or a share that is not there.
	if b, err := s.offloadBlocker(ctx, item, req, plan); err != nil {
		return nil, err
	} else if b != nil {
		p.Blocked = b
	}
	p.Cinema, p.CinemaWarning = s.offloadCinemaLine(ctx)
	plan.identities = make(map[string]string, len(plan.files))
	plan.parentIdentities = make(map[string]string)
	for _, f := range plan.files {
		plan.identities[f.Path] = offloadPathIdentity(f.Path, true)
		parent := filepath.Dir(f.Path)
		if _, pinned := plan.parentIdentities[parent]; !pinned {
			plan.parentIdentities[parent] = offloadPathIdentity(parent, false)
		}
	}
	plan.rootIdentities = make(map[string]string, len(plan.roots))
	for _, root := range plan.roots {
		plan.rootIdentities[root] = offloadPathIdentity(root, false)
	}
	p.Fingerprint = offloadFingerprint(req, plan)
	switch {
	case len(plan.files) == 0:
		p.Summary = "Offload (nothing on disk; it only stops downloads)"
	default:
		p.Summary = fmt.Sprintf("Offload %d file%s, free %s", countMedia(plan.files), pluralS(countMedia(plan.files)), humanBytes(p.Freed))
	}
	return plan, nil
}

func countMedia(files []sqlite.OffloadFile) int {
	n := 0
	for _, f := range files {
		if f.Kind == "media" {
			n++
		}
	}
	return n
}

type offloadSidecar struct {
	path string
	size int64
}

// offloadSidecars lists the files beside video that share its stem and are
// not media (subtitles, .nfo): they go with it. Anything the library holds as
// a file, and any other video or book, is never a sidecar.
func (s *Service) offloadSidecars(ctx context.Context, video string, inLibrary map[string]bool) []offloadSidecar {
	dir := filepath.Dir(video)
	stem := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []offloadSidecar
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || name == filepath.Base(video) || !strings.HasPrefix(name, stem+".") {
			continue
		}
		full := filepath.Join(dir, name)
		if filename.IsVideo(name) || filename.IsBook(name) || inLibrary[full] || s.libraryHasPath(ctx, full) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, offloadSidecar{path: full, size: info.Size()})
	}
	return out
}

func offloadRootOf(roots []domain.RootFolder, path string) string {
	best := ""
	for _, r := range roots {
		root := strings.TrimSuffix(r.Path, "/")
		if (path == r.Path || strings.HasPrefix(path, root+"/")) && len(r.Path) > len(best) {
			best = r.Path
		}
	}
	return best
}

func offloadFingerprint(req OffloadRequest, plan *offloadPlan) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%d|%d|%v|%v|%q|%q\n", req.ItemID, plan.preview.Scope, req.Season, req.EpisodeID, plan.movie, plan.seasons, plan.item.Path, plan.preview.Cinema)
	fmt.Fprintf(h, "recipients|%s\n", plan.revision.Recipients)
	for _, root := range plan.roots {
		fmt.Fprintf(h, "root|%q|%s\n", root, plan.rootIdentities[root])
	}
	for _, copy := range plan.item.Copies {
		fmt.Fprintf(h, "copy|%d|%d|%q\n", copy.ID, copy.RootFolderID, copy.Path)
	}
	for _, id := range plan.episodes {
		fmt.Fprintf(h, "e%d\n", id)
	}
	for i, f := range plan.files {
		episodes := append([]int64(nil), plan.fileEps[f.Path]...)
		sort.Slice(episodes, func(i, j int) bool { return episodes[i] < episodes[j] })
		fmt.Fprintf(h, "%s|%q|%d|%d|%d|%v|%s|%s|%s|%t\n", f.Kind, f.Path, f.Size, f.FileID, plan.fileCopies[f.Path], episodes,
			plan.fileGenerations[f.Path], plan.identities[f.Path], plan.parentIdentities[filepath.Dir(f.Path)], plan.preview.Files[i].Shared)
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// offloadMountProblem says why the library volume cannot be trusted to hold
// the files right now ("" = fine): a root that cannot be listed or is empty
// (an unmounted share shows as an empty mount point). Nothing is deleted, and
// no ENOENT is believed, while this is not empty.
func (s *Service) offloadMountProblem(roots []string) string {
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			return fmt.Sprintf("cannot list %s (%v); the share may be unmounted", root, err)
		}
		if len(entries) == 0 {
			return fmt.Sprintf("%s is empty; the share may be unmounted", root)
		}
	}
	return ""
}

// offloadBlocker is what keeps an Offload of this scope from running now.
func (s *Service) offloadBlocker(ctx context.Context, item domain.MediaItem, req OffloadRequest, plan *offloadPlan) (*OffloadBlocked, error) {
	bl, err := s.db.RematchBlockers(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	scopeEpisodes := map[int64]bool{}
	for _, id := range plan.episodes {
		scopeEpisodes[id] = true
	}
	for _, b := range bl {
		if b.Reason == "download" && item.Kind == domain.KindSeries && req.Scope != "item" && req.Scope != "" {
			// A download for another season or episode does not hold this one.
			if !s.downloadTouches(ctx, b.Ref, scopeEpisodes) {
				continue
			}
		}
		return &OffloadBlocked{Reason: b.Reason, Ref: b.Ref, Retry: true, Message: rematchBlockedText(b, item, item)}, nil
	}
	if why := s.offloadMountProblem(plan.roots); why != "" {
		return &OffloadBlocked{Reason: "unmounted", Retry: true, Message: "The library share is not readable: " + why + ". Nothing is deleted until it is back"}, nil
	}
	return nil, nil
}

// downloadTouches reports whether the download (Activity id) holds any of the
// episodes; a download that names none holds the whole title.
func (s *Service) downloadTouches(ctx context.Context, ref string, episodes map[int64]bool) bool {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return true
	}
	rows, err := s.db.ListDownloadReservations(ctx)
	if err != nil {
		return true
	}
	for _, dl := range rows {
		if dl.ID != id {
			continue
		}
		if len(dl.ReservedEpisodes) == 0 {
			return true
		}
		for _, ep := range dl.ReservedEpisodes {
			if episodes[ep] {
				return true
			}
		}
		return false
	}
	return true
}

// offloadCinemaLine says what every Cinema notifier will do with the
// Offload. A Cinema that does not advertise offload would turn the removal
// into a delete of the title and its watch history: said beforehand, never a
// gate.
func (s *Service) offloadCinemaLine(ctx context.Context) (string, bool) {
	notifiers, err := s.db.ListNotifiers(ctx)
	if err != nil {
		return "", false
	}
	var keep, drop []string
	for _, cfg := range notifiers {
		if !cfg.Enabled || !cfg.OnImport || !ports.QueuedNotifier(cfg) {
			continue
		}
		if cfg.HasFeature(ports.FeatureOffload) {
			keep = append(keep, cfg.Name)
		} else {
			drop = append(drop, cfg.Name)
		}
	}
	switch {
	case len(drop) > 0:
		return fmt.Sprintf("%s does not advertise offload yet: it will drop the title and its watch history. Upgrade it first to keep them", strings.Join(drop, ", ")), true
	case len(keep) > 0:
		return "Cinema keeps the title, its poster and its watch history, greyed out with a Get it back button", false
	}
	return "", false
}

// StartOffload persists a confirmed intent and returns its operation ID.
func (s *Service) StartOffload(ctx context.Context, req OffloadRequest) (sqlite.OffloadOperation, error) {
	if req.RequestID == "" || req.PlanFingerprint == "" {
		return sqlite.OffloadOperation{}, fmt.Errorf("%w: requestId and the preview's fingerprint are required", ErrInvalidOffload)
	}
	if op, err := s.db.OffloadOperationByRequest(ctx, req.RequestID); err == nil {
		if op.ItemID != req.ItemID || op.Fingerprint != req.PlanFingerprint {
			return sqlite.OffloadOperation{}, sqlite.ErrOffloadRequestConflict
		}
		return op, nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.OffloadOperation{}, err
	}
	workCtx, releaseWork, err := ports.TryWork(ctx, s.admission, "queue offload")
	if err != nil {
		return sqlite.OffloadOperation{}, err
	}
	defer releaseWork()
	rec, _, err := s.reviewedOffload(workCtx, req)
	if err != nil {
		if errors.Is(err, ErrInvalidOffload) || errors.Is(err, ErrOffloadStalePreview) {
			if linked, linkErr := s.db.LinkEquivalentOffload(workCtx, req.ItemID, req.RequestID, req.PlanFingerprint); !errors.Is(linkErr, sqlite.ErrNotFound) {
				return linked, linkErr
			}
		}
		return sqlite.OffloadOperation{}, err
	}
	op, _, err := s.db.CreateOffloadOperation(workCtx, rec)
	if err != nil {
		return op, err
	}
	s.driveOffload(s.rematchBase(), op)
	return op, nil
}

func (s *Service) reviewedOffload(ctx context.Context, req OffloadRequest) (sqlite.OffloadRecord, *offloadPlan, error) {
	plan, err := s.planOffload(ctx, req)
	if err != nil {
		return sqlite.OffloadRecord{}, nil, err
	}
	p := plan.preview
	if why := s.offloadMountProblem(plan.roots); why != "" {
		return sqlite.OffloadRecord{}, nil, &OffloadBlockedError{Blocked: OffloadBlocked{
			Reason: "unmounted", Retry: true, Message: "The library share is not readable: " + why,
		}}
	}
	if p.Fingerprint != req.PlanFingerprint {
		return sqlite.OffloadRecord{}, nil, ErrOffloadStalePreview
	}
	stored := offloadStored{Request: req, Label: p.Label, Title: plan.item.Title, Year: plan.item.Year, Kind: string(plan.item.Kind),
		Movie: plan.movie, Seasons: plan.seasons, Episodes: plan.episodes, Fileless: plan.fileless, FileEpisodes: plan.fileEps,
		FileCopies: plan.fileCopies, FileGenerations: plan.fileGenerations, Identities: plan.identities, RootIdentities: plan.rootIdentities, ParentIdentities: plan.parentIdentities, Labels: p.Episodes, Roots: plan.roots, Folder: plan.item.Path}
	stored.Request.RequestID, stored.Request.PlanFingerprint = "", ""
	raw, err := json.Marshal(stored)
	if err != nil {
		return sqlite.OffloadRecord{}, nil, err
	}
	return sqlite.OffloadRecord{
		Op: sqlite.OffloadOperation{ID: newOffloadID(), RequestID: req.RequestID, Fingerprint: req.PlanFingerprint, State: "queued", ItemID: req.ItemID,
			Scope: p.Scope, Plan: raw, FilesTotal: len(plan.files), BytesTotal: p.Bytes},
		Files: plan.files, Movie: plan.movie, Whole: plan.whole, Seasons: plan.seasons, Episodes: plan.episodes,
	}, plan, nil
}

func (s *Service) offloadActivation(ctx context.Context, op sqlite.OffloadOperation, plan *offloadPlan) (sqlite.OffloadRecord, error) {
	var media []string
	for _, f := range plan.files {
		if f.Kind == "media" {
			media = append(media, f.Path)
		}
	}
	outbox, err := s.offloadOutbox(ctx, op.ID, plan.item, media, true)
	return sqlite.OffloadRecord{Op: op, Movie: plan.movie, Whole: plan.whole, Seasons: plan.seasons, Episodes: plan.episodes, Outbox: outbox, Revision: plan.revision}, err
}

// activateQueuedOffload runs with the title and volume authorities held.
func (s *Service) activateQueuedOffload(ctx context.Context, op sqlite.OffloadOperation, stored offloadStored) (bool, error) {
	if why := s.offloadMountProblem(stored.Roots); why != "" {
		return false, s.db.SetQueuedOffloadReason(ctx, op.ID, "Waiting for the library share: "+why)
	}
	reserveCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	unlock, err := s.reserveItem(reserveCtx, op.ItemID)
	cancel()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return false, s.db.SetQueuedOffloadReason(ctx, op.ID, "Waiting for acquisition work on this title")
		}
		return false, err
	}
	defer unlock()
	req := stored.Request
	req.ItemID = op.ItemID
	plan, err := s.planOffload(ctx, req)
	if err != nil {
		if errors.Is(err, ErrInvalidOffload) || errors.Is(err, sqlite.ErrNotFound) {
			return false, s.db.ReviewQueuedOffload(ctx, op.ID, "The reviewed scope changed. Preview it again.")
		}
		return false, err
	}
	if plan.preview.Blocked != nil {
		return false, s.db.SetQueuedOffloadReason(ctx, op.ID, plan.preview.Blocked.Message)
	}
	if plan.preview.Fingerprint != op.Fingerprint {
		return false, s.db.ReviewQueuedOffload(ctx, op.ID, "The reviewed scope or files changed. Preview them again.")
	}
	for _, identity := range plan.identities {
		if identity == "" {
			return false, s.db.ReviewQueuedOffload(ctx, op.ID, "A reviewed file could not be identified. Preview the scope again.")
		}
	}
	rec, err := s.offloadActivation(ctx, op, plan)
	if err != nil {
		return false, err
	}
	started, err := s.db.ActivateOffload(ctx, rec)
	if err == nil && started {
		s.InvalidateWanted()
	}
	return started, err
}

func newOffloadID() string {
	var b [8]byte
	_, _ = io.ReadFull(rand.Reader, b[:])
	return "of-" + hex.EncodeToString(b[:])
}

// reserveItem takes the per-copy acquisition reservation of every copy of the
// item, in ascending copy order, so an automatic grab, a Re-acquire and an
// Offload of the item never interleave.
func (s *Service) reserveItem(ctx context.Context, itemID int64) (func(), error) {
	copies, err := s.db.ListMediaCopies(ctx, itemID)
	if err != nil {
		return nil, err
	}
	ids := []int64{0}
	for _, c := range copies {
		ids = append(ids, c.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var unlocks []func()
	for _, id := range ids {
		unlock, err := s.reservations.acquire(ctx, domain.MovieWantable{Item: itemID, Copy: id})
		if err != nil {
			for _, u := range unlocks {
				u()
			}
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}, nil
}

// launchOffload drives one operation within the process and volume limits.
func (s *Service) launchOffload(ctx context.Context, op sqlite.OffloadOperation, retryNow bool, release func()) bool {
	var stored offloadStored
	_ = json.Unmarshal(op.Plan, &stored)
	var volumes []string
	for _, root := range stored.Roots {
		id := offloadPathIdentity(root, false)
		volume, _, _ := strings.Cut(id, ":")
		volumes = append(volumes, volume)
	}
	s.rematchMu.Lock()
	if s.offloadVolumes == nil {
		s.offloadVolumes = map[string][]string{}
	}
	_, running := s.offloadVolumes[op.ID]
	_, deferred := s.offloadDeferred[op.ID]
	full := len(s.offloadVolumes) >= offloadParallelism
	for _, volume := range volumes {
		count := 0
		for _, active := range s.offloadVolumes {
			for _, v := range active {
				if v == volume {
					count++
					break
				}
			}
		}
		full = full || count >= offloadVolumeParallelism
	}
	if running || full || deferred && !retryNow {
		s.rematchMu.Unlock()
		release()
		return false
	}
	s.offloadVolumes[op.ID] = volumes
	s.rematchMu.Unlock()
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		defer func() {
			current, err := s.db.GetOffloadOperation(ctx, op.ID)
			s.rematchMu.Lock()
			if err != nil || sqlite.OffloadOpen(current.State) {
				if s.offloadDeferred == nil {
					s.offloadDeferred = make(map[string]struct{})
				}
				s.offloadDeferred[op.ID] = struct{}{}
			}
			delete(s.offloadVolumes, op.ID)
			s.rematchMu.Unlock()
			release()
			if ctx.Err() == nil {
				s.dispatchOffloads(s.rematchBase())
			}
		}()
		if err := s.runOffload(ctx, op.ID); err != nil {
			s.log.Warn("offload is not finished; it is retried", "operation", op.ID, "err", err)
		}
	}()
	return true
}

func (s *Service) offloadRunning(id string) bool {
	s.rematchMu.Lock()
	defer s.rematchMu.Unlock()
	_, ok := s.offloadVolumes[id]
	return ok
}

// driveOffloads retries unfinished attempts once per Re-match worker tick.
func (s *Service) driveOffloads(ctx context.Context) {
	s.rematchMu.Lock()
	clear(s.offloadDeferred)
	s.rematchMu.Unlock()
	s.dispatchOffloads(ctx)
}

// dispatchOffloads fills free slots without immediately retrying the work that
// released them. The title head check still orders work within each title.
func (s *Service) dispatchOffloads(ctx context.Context) {
	ops, err := s.db.OpenOffloadOperations(ctx)
	if err != nil {
		s.log.Warn("offload operations unreadable; retried on the next pass", "err", err)
		return
	}
	for _, op := range ops {
		if ctx.Err() != nil {
			return
		}
		s.rematchMu.Lock()
		_, deferred := s.offloadDeferred[op.ID]
		s.rematchMu.Unlock()
		if !deferred && !s.tryDriveOffload(ctx, op, false) {
			return // paused for a drain: resumed after it
		}
	}
}

// driveOffload starts one open operation this process is not driving. It
// reports false when work is paused for a drain.
func (s *Service) driveOffload(ctx context.Context, op sqlite.OffloadOperation) bool {
	// An explicit Stop can settle immediately after a failed delete attempt.
	return s.tryDriveOffload(ctx, op, op.State == "stopping")
}

func (s *Service) tryDriveOffload(ctx context.Context, op sqlite.OffloadOperation, retryNow bool) bool {
	if !sqlite.OffloadOpen(op.State) || s.offloadRunning(op.ID) {
		return true
	}
	if head, err := s.db.HeadOffloadForItem(ctx, op.ItemID); err != nil || head != op.ID {
		return true
	}
	workCtx, releaseWork, err := ports.TryWork(ctx, s.admission, "offload")
	if err != nil {
		return false
	}
	leaseCtx, release, ok := s.tryLockImportTarget(workCtx, op.ItemID)
	if !ok {
		releaseWork()
		if op.State == "queued" {
			_ = s.db.SetQueuedOffloadReason(ctx, op.ID, "Waiting for work on this title")
		}
		return true
	}
	s.launchOffload(leaseCtx, op, retryNow, func() { release(); releaseWork() })
	return true
}

// runOffload drives one operation from wherever it stands; the caller holds
// the title. An error leaves the operation where it is, for the next pass.
func (s *Service) runOffload(ctx context.Context, id string) error {
	bg := context.WithoutCancel(ctx)
	op, err := s.db.GetOffloadOperation(bg, id)
	if err != nil {
		return fmt.Errorf("read operation: %w", err)
	}
	if !sqlite.OffloadOpen(op.State) {
		return nil
	}
	var stored offloadStored
	if err := json.Unmarshal(op.Plan, &stored); err != nil {
		if op.State == "queued" {
			return s.db.ReviewQueuedOffload(bg, id, "Unreadable reviewed plan. Preview the scope again.")
		}
		return s.db.FailOffload(bg, id, "unreadable plan: "+err.Error())
	}
	if op.State == "queued" {
		started, err := s.activateQueuedOffload(ctx, op, stored)
		if err != nil || !started {
			return err
		}
		op.State = "deleting"
	}
	if op.State == "stopping" {
		return s.finishStop(bg, op, stored) // a Stop the last process did not finish
	}
	files, err := s.db.ListOffloadFiles(bg, id)
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	// The share first: nothing is deleted, and no missing file is believed
	// to be gone, while the library volume is not readable.
	if why := s.offloadMountProblem(stored.Roots); why != "" {
		return s.db.SetOffloadState(bg, id, "waiting", "waiting for the library share: "+why)
	}
	for root, expected := range stored.RootIdentities {
		if expected == "" || offloadPathIdentity(root, false) != expected {
			return s.db.SetOffloadState(bg, id, "waiting", "Waiting for the reviewed library volume at "+root)
		}
	}
	if op.State == "waiting" {
		if err := s.db.SetOffloadState(bg, id, "deleting", ""); err != nil {
			return err
		}
	}
	// Media files first, so the item page empties at once; sidecars after.
	sort.SliceStable(files, func(i, j int) bool { return files[i].Kind == "media" && files[j].Kind != "media" })
	var failed []string
	var firstErr string
	for _, f := range files {
		if f.State == "deleted" {
			continue
		}
		if ctx.Err() != nil {
			return nil // shutting down: the journal resumes it
		}
		if cur, err := s.db.GetOffloadOperation(bg, id); err != nil || !sqlite.OffloadWorking(cur.State) {
			if err == nil && cur.State == "stopping" {
				// Stop: the remaining deletes do not run. This runner is
				// the one that finishes it, so no delete is in flight.
				return s.finishStop(bg, cur, stored)
			}
			return err
		}
		freed, err := s.offloadDeleteFile(bg, op, stored, f)
		if errors.Is(err, errOffloadStopped) {
			return nil
		}
		if err != nil {
			_ = s.db.MarkOffloadFile(bg, id, f.Path, "failed", err.Error(), 0)
			failed = append(failed, f.Path)
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if err := s.db.RecordOffloadFileRemoved(bg, op.ItemID, id, f, stored.FileGenerations[f.Path], freed); err != nil {
			return fmt.Errorf("journal %s: %w", f.Path, err)
		}
	}
	if cur, err := s.db.GetOffloadOperation(bg, id); err == nil && cur.State == "stopping" {
		return s.finishStop(bg, cur, stored)
	}
	if len(failed) > 0 {
		reason := fmt.Sprintf("%d file%s could not be deleted (%s); retrying", len(failed), pluralS(len(failed)), firstErr)
		return s.db.SetOffloadState(bg, id, "retrying", reason)
	}
	return s.finishOffload(bg, id, stored)
}

// openOffloadParent binds the final lookup and unlink to the reviewed storage
// root and directory, even if an ancestor name is replaced after opening.
func openOffloadParent(stored offloadStored, path string) (*os.Root, error) {
	parentPath := filepath.Dir(path)
	wantParent := stored.ParentIdentities[parentPath]
	var rootPath, rel string
	for _, candidate := range stored.Roots {
		r, err := filepath.Rel(candidate, parentPath)
		if err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && len(candidate) > len(rootPath) {
			rootPath, rel = candidate, r
		}
	}
	wantRoot := stored.RootIdentities[rootPath]
	if wantParent == "" || wantRoot == "" {
		return nil, fmt.Errorf("reviewed storage identity is unavailable for %s", path)
	}
	if err := rejectSymlinks(rootPath, false); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil || offloadFileIdentity(rootInfo, false) != wantRoot {
		return nil, fmt.Errorf("reviewed library volume changed at %s", rootPath)
	}
	parent, err := openDeliveryDirectoryNoSymlinks(root, rel)
	if err != nil {
		return nil, err
	}
	info, err := parent.Stat(".")
	if err != nil || offloadFileIdentity(info, false) != wantParent {
		_ = parent.Close()
		return nil, fmt.Errorf("reviewed directory changed at %s", parentPath)
	}
	return parent, nil
}

var errOffloadStopped = errors.New("offload stopped before unlink")

func (s *Service) offloadDeleteFile(ctx context.Context, op sqlite.OffloadOperation, stored offloadStored, f sqlite.OffloadFile) (int64, error) {
	expected := stored.Identities[f.Path]
	if expected == "" {
		return 0, fmt.Errorf("reviewed file identity is unavailable for %s; stop and review again", f.Path)
	}
	parent, err := openOffloadParent(stored, f.Path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = parent.Close() }()
	name := filepath.Base(f.Path)
	info, err := parent.Lstat(name)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		return 0, err
	}
	custody, err := s.db.GetMediaFileCustodyByPath(ctx, f.Path)
	if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
		return 0, err
	}
	if err == nil && (f.Kind != "media" || custody.MediaItemID != op.ItemID || custody.ID != f.FileID ||
		custody.FileGeneration != stored.FileGenerations[f.Path]) {
		return 0, fmt.Errorf("library ownership changed at %s", f.Path)
	}
	if absent {
		// Only infer replay from absence inside the exact reviewed directory.
		return 0, syncRootDirectory(parent, ".")
	}
	if f.Kind == "media" && errors.Is(err, sqlite.ErrNotFound) {
		return 0, fmt.Errorf("library ownership is missing at %s", f.Path)
	}
	if offloadFileIdentity(info, true) != expected ||
		offloadPathIdentity(filepath.Dir(f.Path), false) != stored.ParentIdentities[filepath.Dir(f.Path)] {
		return 0, fmt.Errorf("file or directory identity changed at %s", f.Path)
	}
	ready, err := s.db.BeginOffloadUnlink(ctx, op.ID, f.Path)
	if err != nil {
		return 0, err
	}
	if !ready {
		return 0, errOffloadStopped
	}
	current, err := parent.Lstat(name)
	if err != nil || offloadFileIdentity(current, true) != expected {
		return 0, fmt.Errorf("file identity changed before removing %s", f.Path)
	}
	if err := parent.Remove(name); err != nil {
		return 0, fmt.Errorf("delete %s: %w", f.Path, err)
	}
	if err := syncRootDirectory(parent, "."); err != nil {
		return 0, err
	}
	if offloadInfoLinkCount(current) > 1 {
		return 0, nil
	}
	return current.Size(), nil
}

func (s *Service) finishOffload(ctx context.Context, id string, stored offloadStored) error {
	op, err := s.db.GetOffloadOperation(ctx, id)
	if err != nil {
		return err
	}
	files, err := s.db.ListOffloadFiles(ctx, id)
	if err != nil {
		return err
	}
	res := offloadResult{Episodes: stored.Labels, Bytes: op.BytesFreed}
	for _, f := range files {
		if f.State == "deleted" {
			res.Files++
			if f.Kind == "media" {
				res.Media++
			}
		}
	}
	raw, _ := json.Marshal(res)
	h := sqlite.HistoryWrite{Type: HistoryOffloaded, MediaItemID: op.ItemID, Data: map[string]any{
		"operation": id, "scope": op.Scope, "label": stored.Label, "files": res.Files, "bytes": res.Bytes, "episodes": stored.Labels}}
	if err := s.db.CompleteOffload(ctx, id, raw, h); err != nil {
		return err
	}
	s.InvalidateWanted()
	s.log.Info("offload complete", "operation", id, "files", res.Files, "bytes", res.Bytes)
	return nil
}

// StopOffload ends the remaining deletes. It flips the operation to
// stopping; the worker finishes it once the runner is out (so no file is
// deleted unrecorded), clearing what still has a file and rewriting Cinema's
// outbox to what was deleted, in one transaction that a restart resumes.
// Files already deleted stay deleted. It returns when the stop is finished,
// or after a bounded wait (the status says stopping).
func (s *Service) StopOffload(ctx context.Context, id string) error {
	op, err := s.db.GetOffloadOperation(ctx, id)
	if err != nil {
		return err
	}
	if !sqlite.OffloadOpen(op.State) {
		return fmt.Errorf("%w: it has already finished", ErrInvalidOffload)
	}
	if _, err := s.db.RequestStopOffload(ctx, id); err != nil {
		return err
	}
	deadline := time.Now().Add(offloadStopWait)
	for {
		cur, err := s.db.GetOffloadOperation(ctx, id)
		if err != nil || !sqlite.OffloadOpen(cur.State) {
			s.InvalidateWanted()
			s.dispatchOffloads(s.rematchBase())
			return err
		}
		if !s.offloadRunning(id) {
			s.driveOffload(s.rematchBase(), cur)
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(25 * time.Millisecond):
		}
	}
}

const offloadStopWait = 5 * time.Second

// finishStop completes a Stop; the caller holds the title and no delete is in
// flight. Idempotent: a crash anywhere before its one transaction repeats it.
func (s *Service) finishStop(ctx context.Context, op sqlite.OffloadOperation, stored offloadStored) error {
	files, err := s.db.ListOffloadFiles(ctx, op.ID)
	if err != nil {
		return err
	}
	if cur, err := s.db.GetOffloadOperation(ctx, op.ID); err == nil {
		op = cur
	}
	for i := range files {
		f := &files[i]
		if !f.UnlinkStarted || f.State == "deleted" {
			continue
		}
		parent, err := openOffloadParent(stored, f.Path)
		if err != nil {
			return fmt.Errorf("waiting to settle interrupted unlink: %w", err)
		}
		_, statErr := parent.Lstat(filepath.Base(f.Path))
		if errors.Is(statErr, os.ErrNotExist) {
			err = syncRootDirectory(parent, ".")
		}
		_ = parent.Close()
		if err != nil {
			return err
		}
		if statErr == nil {
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if err := s.db.RecordOffloadFileRemoved(ctx, op.ItemID, op.ID, *f, stored.FileGenerations[f.Path], 0); err != nil {
			return err
		}
		f.State = "deleted"
	}
	// Bytes released, not file sizes: a hard-linked or already-missing file
	// freed nothing (the preview's Freed counts the same way).
	res := offloadResult{Episodes: stored.Labels, Bytes: op.BytesFreed}
	var deleted []string
	for _, f := range files {
		if f.State == "deleted" {
			res.Files++
			if f.Kind == "media" {
				res.Media++
				deleted = append(deleted, f.Path)
			}
		}
	}
	raw, _ := json.Marshal(res)
	st := sqlite.OffloadStop{Result: raw, Fileless: stored.Fileless, Episodes: stored.Episodes, Seasons: stored.Seasons,
		ClearItem: op.Scope == "item" && (!stored.Movie || res.Media == 0)}
	if res.Files > 0 {
		st.History = &sqlite.HistoryWrite{Type: HistoryOffloaded, MediaItemID: op.ItemID, Data: map[string]any{
			"operation": op.ID, "scope": op.Scope, "label": stored.Label, "files": res.Files, "bytes": res.Bytes, "episodes": stored.Labels, "stopped": true}}
	}
	// Cinema is told only what was deleted.
	if len(deleted) > 0 {
		item, err := s.db.GetMediaItemFull(ctx, op.ItemID)
		if err != nil {
			return err
		}
		if st.Keep, err = s.offloadOutbox(ctx, op.ID, item, deleted, false); err != nil {
			return err
		}
	}
	if _, err := s.db.FinishStopOffload(ctx, op.ID, st); err != nil {
		return err
	}
	s.InvalidateWanted()
	s.log.Info("offload stopped", "operation", op.ID, "files", res.Files)
	return nil
}

// DismissOffload hides a finished operation from the status panel.
func (s *Service) DismissOffload(ctx context.Context, id string) error {
	ok, err := s.db.DismissOffload(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := s.db.GetOffloadOperation(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("%w: it is still running; Stop it instead", ErrInvalidOffload)
	}
	return nil
}

// OffloadStatus is GET /offload/{id}: the operation and what Cinema said.
type OffloadStatus struct {
	sqlite.OffloadOperation
	Label      string          `json:"label,omitempty"`
	Season     *int            `json:"season,omitempty"`
	EpisodeID  int64           `json:"episodeId,omitempty"`
	FilesDone  int             `json:"filesDone"`
	Failed     []string        `json:"failed,omitempty"`
	Stoppable  bool            `json:"stoppable"`
	Cinema     []RematchCinema `json:"cinema"`
	CinemaLine string          `json:"cinemaLine,omitempty"`
}

// GetOffload reads one operation.
func (s *Service) GetOffload(ctx context.Context, id string) (OffloadStatus, error) {
	op, err := s.db.GetOffloadOperation(ctx, id)
	if err != nil {
		return OffloadStatus{}, err
	}
	return s.offloadStatus(ctx, op)
}

func (s *Service) offloadStatus(ctx context.Context, op sqlite.OffloadOperation) (OffloadStatus, error) {
	id := op.ID
	out := OffloadStatus{OffloadOperation: op, Cinema: []RematchCinema{}, Stoppable: op.State == "queued" || sqlite.OffloadWorking(op.State)}
	var stored offloadStored
	_ = json.Unmarshal(op.Plan, &stored)
	out.Label = stored.Label
	if op.Scope == "season" {
		out.Season = &stored.Request.Season
	}
	out.EpisodeID = stored.Request.EpisodeID
	files, err := s.db.ListOffloadFiles(ctx, id)
	if err != nil {
		return out, err
	}
	for _, f := range files {
		switch f.State {
		case "deleted":
			out.FilesDone++
		case "failed":
			out.Failed = append(out.Failed, f.Path+": "+f.Error)
		}
	}
	deliveries, err := s.db.OperationDeliveries(ctx, id)
	if err != nil {
		return out, err
	}
	confirmed := true
	notTold := ""
	var said []string
	for _, d := range deliveries {
		if d.IdempotencyKey == "" || d.Status == "failed" && strings.HasPrefix(d.LastError, "cancelled") {
			continue
		}
		c := RematchCinemaOf(d)
		out.Cinema = append(out.Cinema, c)
		if c.Status == "ok" && c.VisibleAt == 0 && len(c.Warnings) > 0 {
			// Settled without being sent: a lasting warning, never "In cinema".
			notTold = c.Warnings[0]
			continue
		}
		confirmed = confirmed && c.VisibleAt > 0
		if c.Result != "" {
			said = append(said, c.Result)
		}
	}
	switch {
	case len(out.Cinema) == 0:
	case notTold != "" && op.State == "complete":
		out.CinemaLine = "Cinema was not told: " + strings.TrimPrefix(notTold, "cinema not told: ")
	case op.State == "complete" && confirmed:
		out.CinemaLine = "In cinema: " + strings.Join(said, "; ")
	case op.State == "complete":
		out.CinemaLine = "Cinema is being told."
	default:
		out.CinemaLine = "Cinema is told once the files are gone."
	}
	return out, nil
}

// offloadOutbox writes the Offload's notifications. Cinema gets one
// operation-keyed sub-delivery per library root and chunk (contract 4.5), every
// one probe-only: offload when it advertises "offload", plain removed paths
// otherwise (the dialog said beforehand it will drop the title). hold keeps
// them from being sent until the files are gone.
func (s *Service) offloadOutbox(ctx context.Context, opID string, item domain.MediaItem, paths []string, hold bool) ([]sqlite.OutboxWrite, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	notifiers, err := s.db.ListNotifiers(ctx)
	if err != nil {
		return nil, err
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	groups := map[string][]rematchPair{}
	for _, p := range paths {
		root := offloadRootOf(roots, p)
		if root == "" {
			root = filepath.Dir(p)
		}
		groups[root] = append(groups[root], rematchPair{rem: []string{p}})
	}
	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []sqlite.OutboxWrite
	for _, cfg := range notifiers {
		if !cfg.Enabled || !cfg.OnImport || !ports.QueuedNotifier(cfg) {
			continue
		}
		// Always `offload` with offloaded paths: the adapter downgrades at
		// send time from the features it re-reads then (offload, removed
		// paths as a move, or nothing), so a Cinema upgraded or downgraded
		// after Start is still told the right way.
		const event = "offload"
		for _, root := range keys {
			var all []string
			for _, pr := range groups[root] {
				all = append(all, pr.rem...)
			}
			dir := commonParent(all)
			if dir == "" || !strings.HasPrefix(dir+"/", strings.TrimSuffix(root, "/")+"/") {
				dir = root
			}
			for chunk, pairs := range chunkPairs(groups[root]) {
				var part []string
				for _, pr := range pairs {
					part = append(part, pr.rem...)
				}
				info := ports.ImportInfo{MediaItemID: item.ID, Dirs: []string{dir}, Kind: string(item.Kind), Title: item.Title,
					TmdbID: item.IDs.TMDB, ImdbID: item.IDs.IMDB}
				info.OffloadedPaths = part
				n := ports.Notification{Event: event, Title: "Offloaded: " + item.Title,
					Body:   fmt.Sprintf("%d file%s offloaded from %s", len(part), pluralS(len(part)), item.Title),
					Fields: map[string]string{"title": item.Title}, Import: &info}
				payload, err := json.Marshal(n)
				if err != nil {
					return nil, err
				}
				key := RematchDeliveryKey(opID, dir)
				if chunk > 0 {
					key += "-" + strconv.Itoa(chunk)
				}
				out = append(out, sqlite.OutboxWrite{NotifierID: cfg.ID, MediaItemID: item.ID, Directory: dir, IdempotencyKey: key,
					Event: event, Payload: string(payload), OperationID: opID, Hold: hold})
			}
		}
	}
	return out, nil
}

// OffloadForItem is the operation the item page follows: the open one, else
// the newest finished one that was not dismissed and is still recent.
func (s *Service) OffloadForItem(ctx context.Context, itemID int64) (OffloadStatus, bool, error) {
	id, err := s.db.HeadOffloadForItem(ctx, itemID)
	if err != nil {
		return OffloadStatus{}, false, err
	}
	if id == "" {
		done, ok, err := s.db.LatestEndedOffloadForItem(ctx, itemID, time.Now().Add(-rematchEndedFor).UnixMilli())
		if err != nil || !ok {
			return OffloadStatus{}, false, err
		}
		id = done.ID
	}
	st, err := s.GetOffload(ctx, id)
	return st, err == nil, err
}

// ListOffloadsForItem returns independent operation progress for a title.
func (s *Service) ListOffloadsForItem(ctx context.Context, itemID int64) ([]OffloadStatus, error) {
	ops, err := s.db.OffloadOperationsForItem(ctx, itemID, time.Now().Add(-rematchEndedFor).UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]OffloadStatus, 0, len(ops))
	for _, op := range ops {
		status, err := s.offloadStatus(ctx, op)
		if err != nil {
			return nil, err
		}
		out = append(out, status)
	}
	return out, nil
}
