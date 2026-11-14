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

// OffloadBlockedError is an Offload refused while work is in flight on the
// title, or while its share is not readable (409 offload_blocked).
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
	preview    OffloadPreview
	item       domain.MediaItem
	movie      bool
	whole      bool // item scope of a series: the stored whole-show intent
	seasons    []int
	episodes   []int64 // every episode that becomes offloaded
	fileless   []int64 // those of them with no file at plan time
	fileEps    map[string][]int64
	fileCopies map[string]int64
	files      []sqlite.OffloadFile
	roots      []string
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
	FileCopies   map[string]int64   `json:"fileCopies,omitempty"`
	FileEpisodes map[string][]int64 `json:"fileEpisodes,omitempty"`
	Labels       []string           `json:"episodeLabels,omitempty"`
	Roots        []string           `json:"roots,omitempty"`
	Folder       string             `json:"folder,omitempty"`
}

const offloadLeaseWait = 10 * time.Second

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
	item, err := s.db.GetMediaItemFull(ctx, req.ItemID)
	if err != nil {
		return nil, err
	}
	if item.Kind != domain.KindMovie && item.Kind != domain.KindSeries {
		return nil, fmt.Errorf("%w: only a movie or a show can be offloaded", ErrInvalidOffload)
	}
	plan := &offloadPlan{item: item}
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
		}
		plan.fileCopies[f.Path] = f.CopyID
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
			p.Files = append(p.Files, OffloadFilePlan{Path: sc.path, Kind: "sidecar", Bytes: sc.size})
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
	fmt.Fprintf(h, "%d|%s|%d|%d|%v|%v|%v\n", req.ItemID, plan.preview.Scope, req.Season, req.EpisodeID, plan.movie, plan.seasons, plan.preview.CinemaWarning)
	for _, id := range plan.episodes {
		fmt.Fprintf(h, "e%d\n", id)
	}
	for _, f := range plan.files {
		fmt.Fprintf(h, "%s|%s|%d\n", f.Kind, f.Path, f.Size)
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

// StartOffload executes a previewed Offload. It returns once the operation is
// recorded; the deletes run in the background (GET /offload/{id}), admitted
// as work so a deployment drain sees and waits for them.
func (s *Service) StartOffload(ctx context.Context, req OffloadRequest) (sqlite.OffloadOperation, error) {
	if req.RequestID == "" || req.PlanFingerprint == "" {
		return sqlite.OffloadOperation{}, fmt.Errorf("%w: requestId and the preview's fingerprint are required", ErrInvalidOffload)
	}
	// A retried request answers with the operation it created.
	if op, err := s.db.OffloadOperationByRequest(ctx, req.RequestID); err == nil {
		if op.Fingerprint != req.PlanFingerprint {
			return sqlite.OffloadOperation{}, sqlite.ErrOffloadRequestConflict
		}
		return op, nil
	}
	workCtx, releaseWork, err := ports.TryWork(s.rematchBase(), s.admission, "offload")
	if err != nil {
		return sqlite.OffloadOperation{}, err
	}
	leaseCtx, release, err := s.lockImportTargets(workCtx, offloadLeaseWait, req.ItemID)
	if err != nil {
		releaseWork()
		return sqlite.OffloadOperation{}, err
	}
	op, err := s.startOffloadLocked(leaseCtx, req)
	if err != nil {
		release()
		releaseWork()
		return sqlite.OffloadOperation{}, err
	}
	s.launchOffload(leaseCtx, op.ID, func() { release(); releaseWork() })
	return op, nil
}

func (s *Service) startOffloadLocked(ctx context.Context, req OffloadRequest) (sqlite.OffloadOperation, error) {
	// Re-acquire and Offload serialise on the item's per-copy reservations.
	unlock, err := s.reserveItem(ctx, req.ItemID)
	if err != nil {
		return sqlite.OffloadOperation{}, err
	}
	defer unlock()
	plan, err := s.planOffload(ctx, req)
	if err != nil {
		return sqlite.OffloadOperation{}, err
	}
	p := plan.preview
	if p.Blocked != nil {
		return sqlite.OffloadOperation{}, &OffloadBlockedError{Blocked: *p.Blocked}
	}
	if p.Fingerprint != req.PlanFingerprint {
		return sqlite.OffloadOperation{}, ErrOffloadStalePreview
	}
	opID := newOffloadID()
	stored := offloadStored{Request: req, Label: p.Label, Title: plan.item.Title, Year: plan.item.Year, Kind: string(plan.item.Kind),
		Movie: plan.movie, Seasons: plan.seasons, Episodes: plan.episodes, Fileless: plan.fileless, FileEpisodes: plan.fileEps, FileCopies: plan.fileCopies, Labels: p.Episodes, Roots: plan.roots, Folder: plan.item.Path}
	stored.Request.RequestID, stored.Request.PlanFingerprint = "", ""
	raw, err := json.Marshal(stored)
	if err != nil {
		return sqlite.OffloadOperation{}, err
	}
	var media []string
	for _, f := range plan.files {
		if f.Kind == "media" {
			media = append(media, f.Path)
		}
	}
	outbox, err := s.offloadOutbox(ctx, opID, plan.item, media, true)
	if err != nil {
		return sqlite.OffloadOperation{}, err
	}
	op, existing, err := s.db.CreateOffloadOperation(ctx, sqlite.OffloadRecord{
		Op: sqlite.OffloadOperation{ID: opID, RequestID: req.RequestID, Fingerprint: req.PlanFingerprint, State: "deleting", ItemID: req.ItemID,
			Scope: p.Scope, Plan: raw, FilesTotal: len(plan.files), BytesTotal: p.Bytes},
		Files: plan.files, Movie: plan.movie, Whole: plan.whole, Seasons: plan.seasons, Episodes: plan.episodes, Outbox: outbox,
	})
	if err != nil {
		return op, err
	}
	if !existing {
		s.InvalidateWanted()
		s.log.Info("offload started", "operation", opID, "item", plan.item.Title, "scope", p.Scope, "files", len(plan.files), "bytes", p.Bytes)
	}
	return op, nil
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

// launchOffload drives one operation in the background, once.
func (s *Service) launchOffload(ctx context.Context, id string, release func()) bool {
	s.rematchMu.Lock()
	if s.offloadCancels == nil {
		s.offloadCancels = map[string]context.CancelFunc{}
	}
	if _, running := s.offloadCancels[id]; running {
		s.rematchMu.Unlock()
		release()
		return false
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.offloadCancels[id] = cancel
	s.rematchMu.Unlock()
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		defer release()
		defer func() {
			s.rematchMu.Lock()
			delete(s.offloadCancels, id)
			s.rematchMu.Unlock()
			cancel()
		}()
		if err := s.runOffload(runCtx, id); err != nil {
			s.log.Warn("offload is not finished; it is retried", "operation", id, "err", err)
		}
	}()
	return true
}

func (s *Service) offloadRunning(id string) bool {
	s.rematchMu.Lock()
	defer s.rematchMu.Unlock()
	_, ok := s.offloadCancels[id]
	return ok
}

// driveOffloads starts every open operation this process is not already
// driving: what a restart interrupted, a share that came back, a delete that
// failed. It runs on the Re-match worker's tick.
func (s *Service) driveOffloads(ctx context.Context) {
	ops, err := s.db.OpenOffloadOperations(ctx)
	if err != nil {
		s.log.Warn("offload operations unreadable; retried on the next pass", "err", err)
		return
	}
	for _, op := range ops {
		if ctx.Err() != nil {
			return
		}
		if !s.driveOffload(ctx, op) {
			return // paused for a drain: resumed after it
		}
	}
}

// driveOffload starts one open operation this process is not driving. It
// reports false when work is paused for a drain.
func (s *Service) driveOffload(ctx context.Context, op sqlite.OffloadOperation) bool {
	if s.offloadRunning(op.ID) {
		return true
	}
	workCtx, releaseWork, err := ports.TryWork(ctx, s.admission, "offload")
	if err != nil {
		return false
	}
	leaseCtx, release, err := s.lockImportTargets(workCtx, 2*time.Second, op.ItemID)
	if err != nil {
		releaseWork()
		if time.Since(time.UnixMilli(op.CreatedAt)) >= offloadLeaseWait {
			_ = s.db.SetOffloadState(ctx, op.ID, op.State, "waiting for its title: "+err.Error())
		}
		return true
	}
	s.launchOffload(leaseCtx, op.ID, func() { release(); releaseWork() })
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
		return s.db.FailOffload(bg, id, "unreadable plan: "+err.Error())
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
		freed, err := s.offloadDeleteFile(bg, stored, f)
		if err != nil {
			_ = s.db.MarkOffloadFile(bg, id, f.Path, "failed", err.Error(), 0)
			failed = append(failed, f.Path)
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if err := s.db.MarkOffloadFile(bg, id, f.Path, "deleted", "", freed); err != nil {
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

// offloadDeleteFile removes one journaled file and, for a library file, its
// row. A file already gone is fine only because the share was verified
// readable (and non-empty) at the start of this pass.
func (s *Service) offloadDeleteFile(ctx context.Context, stored offloadStored, f sqlite.OffloadFile) (int64, error) {
	if err := rejectSymlinks(f.Path, true); err != nil {
		return 0, err
	}
	// What this delete actually releases: nothing for a file that is already
	// gone or that other hard links keep alive (the preview's Freed says the same).
	var freed int64
	if info, err := os.Lstat(f.Path); err == nil && offloadLinkCount(f.Path) <= 1 {
		freed = info.Size()
	}
	if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("delete %s: %w", f.Path, err)
	} else if errors.Is(err, os.ErrNotExist) {
		freed = 0
		// Believed only while the share still lists: it may have gone away
		// during the pass.
		if why := s.offloadMountProblem(stored.Roots); why != "" {
			return 0, fmt.Errorf("cannot tell whether %s is gone: %s", f.Path, why)
		}
		// A root that lists proves little when the title's own folder does
		// not: a share mounted one level too high, or a partly mounted tree,
		// answers ENOENT for files that are still there.
		if stored.Folder != "" {
			if _, err := os.ReadDir(stored.Folder); err != nil {
				return 0, fmt.Errorf("cannot tell whether %s is gone: the title's folder %s cannot be listed (%v)", f.Path, stored.Folder, err)
			}
		}
	}
	_ = syncPath(filepath.Dir(f.Path))
	switch f.Kind {
	case "media":
		if f.FileID != 0 {
			if err := s.db.DeleteFile(ctx, f.FileID); err != nil && !errors.Is(err, sqlite.ErrNotFound) {
				return 0, fmt.Errorf("remove the library row of %s: %w", f.Path, err)
			}
		}
	default:
		// A subtitle Curator placed is recorded as superseded, so cleanup does
		// not find its library copy gone.
		if rows, err := s.db.PlacedLibraryFilesWithPrefix(ctx, f.Path); err == nil {
			for _, r := range rows {
				if r.LibraryPath == f.Path {
					s.supersedePlacedSubtitle(ctx, r, "its video was offloaded")
				}
			}
		}
	}
	return freed, nil
}

// OffloadResult is what a finished Offload says it did.
type offloadResult struct {
	Files    int      `json:"files"`
	Media    int      `json:"media"`
	Bytes    int64    `json:"bytes"`
	Episodes []string `json:"episodes,omitempty"`
}

// HistoryOffloaded records an Offload: the scope, how many files and bytes.
const HistoryOffloaded = "offloaded"

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
	st := sqlite.OffloadStop{Result: raw, Fileless: stored.Fileless, ClearItem: !stored.Movie || res.Media == 0}
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
	out := OffloadStatus{OffloadOperation: op, Cinema: []RematchCinema{}, Stoppable: sqlite.OffloadWorking(op.State)}
	var stored offloadStored
	_ = json.Unmarshal(op.Plan, &stored)
	out.Label = stored.Label
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
	id, err := s.db.OpenOffloadForItem(ctx, itemID)
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
