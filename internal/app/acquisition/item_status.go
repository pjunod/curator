package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The item page's status panel (0.44.0): a read projection of where one
// movie or book, or each episode of a series, stands from search to Cinema.
// Like QueuePage and ImportActivity it reads what the pipeline already
// records — search records, download rows, publications, sub-deliveries —
// and decides nothing. Times are returned as Unix ms and never written into
// text: the viewer's clock formats them.
//
// The panel follows the primary copy. Additional quality copies and book
// editions have their own rows in Activity.

// Stage keys, in the order an item passes through them.
const (
	StageSearched       = "searched"
	StageDownloading    = "downloading"
	StageProcessed      = "processed"
	StageMoved          = "moved"
	StageCinemaNotified = "cinema_notified"
	StageInCinema       = "in_cinema"
)

// Stage states.
const (
	StatusDone    = "done"
	StatusActive  = "active"
	StatusWaiting = "waiting"
	StatusFailed  = "failed"
	StatusTodo    = "todo"
)

// Action hints: what the panel may offer beside a stage.
const (
	ActionSearch            = "search"
	ActionInteractiveSearch = "interactive_search"
	ActionOpenActivity      = "open_activity"
	ActionCancelDownload    = "cancel_download"
	ActionRetryImport       = "retry_import"
	ActionResendCinema      = "resend_cinema"
	ActionConfirmCinema     = "confirm_cinema"
)

// Bounds on what one poll reads.
const (
	statusDownloadsRead  = 100
	statusDeliveriesRead = 200
	statusAttemptsShown  = 5
	// statusRateFresh is how old Runner's last numbers may be and still be
	// shown as a rate and an ETA; older, the runner is not reporting.
	statusRateFresh = 2 * time.Minute
)

// StatusStage is one of the six stages.
type StatusStage struct {
	Key     string   `json:"key"`
	State   string   `json:"state"`
	At      int64    `json:"at,omitempty"` // Unix ms; 0 = no time to show
	Detail  string   `json:"detail,omitempty"`
	Actions []string `json:"actions,omitempty"`
}

// StatusDownload is the download in flight (or the last one), with Runner's
// numbers relayed: ETA is remaining/rate, nothing averaged here.
type StatusDownload struct {
	ID         int64   `json:"id"`
	Release    string  `json:"release"`
	Indexer    string  `json:"indexer,omitempty"`
	State      string  `json:"state"`
	Bytes      int64   `json:"bytes"`
	Total      int64   `json:"total"`
	Progress   float64 `json:"progress"`
	RateBps    int64   `json:"rateBps,omitempty"`
	ETASeconds int64   `json:"etaSeconds,omitempty"`
	Stage      string  `json:"stage,omitempty"`
	StageSince int64   `json:"stageSince,omitempty"`
	Episodes   int     `json:"episodes,omitempty"` // >1 = a season pack
}

// StatusAttempt is an earlier grab of the same item.
type StatusAttempt struct {
	DownloadID int64  `json:"downloadId"`
	Release    string `json:"release"`
	Indexer    string `json:"indexer,omitempty"`
	At         int64  `json:"at"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`
}

// StatusCinema names the sub-delivery a cinema action works on.
type StatusCinema struct {
	DeliveryID int64   `json:"deliveryId"`
	NotifierID int64   `json:"notifierId"`
	Status     string  `json:"status"` // pending | ok | failed
	WaitingOn  string  `json:"waitingOn,omitempty"`
	VisibleAt  int64   `json:"visibleAt,omitempty"`
	ItemIDs    []int64 `json:"itemIds,omitempty"`
	Error      string  `json:"error,omitempty"`
	// Warnings are cinema's lasting answers about files that left (a Re-match).
	Warnings []string `json:"warnings,omitempty"`
}

// StatusHeadline is the one line at the top of the panel; At is the time
// the viewer's clock shows beside it.
type StatusHeadline struct {
	State  string `json:"state"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	At     int64  `json:"at,omitempty"`
}

// StatusEpisode is one episode's strip, with what its actions work on.
type StatusEpisode struct {
	EpisodeID  int64         `json:"episodeId"`
	Season     int           `json:"season"`
	Episode    int           `json:"episode"`
	Title      string        `json:"title,omitempty"`
	WantableID string        `json:"wantableId"`
	Summary    string        `json:"summary"`
	At         int64         `json:"at,omitempty"`
	InPack     bool          `json:"inPack"`
	DownloadID int64         `json:"downloadId,omitempty"`
	Cinema     *StatusCinema `json:"cinema,omitempty"`
	Stages     []StatusStage `json:"stages"`
}

// StatusSeason is one season's summary over its monitored, aired episodes;
// its season-pack download is shown here once, not on every episode.
type StatusSeason struct {
	Season   int             `json:"season"`
	Episodes int             `json:"episodes"`
	InCinema int             `json:"inCinema"`
	OnDisk   int             `json:"onDisk"`
	Active   bool            `json:"active"`
	Summary  string          `json:"summary"`
	Download *StatusDownload `json:"download,omitempty"`
}

// ItemStatus is GET /library/{id}/status.
type ItemStatus struct {
	MediaItemID int64           `json:"mediaItemId"`
	Kind        string          `json:"kind"`
	GeneratedAt int64           `json:"generatedAt"`
	Active      bool            `json:"active"` // a download is moving: poll fast
	Done        bool            `json:"done"`   // every stage done: the panel may collapse
	Headline    StatusHeadline  `json:"headline"`
	Stages      []StatusStage   `json:"stages,omitempty"`
	Download    *StatusDownload `json:"download,omitempty"`
	Cinema      *StatusCinema   `json:"cinema,omitempty"`
	Attempts    []StatusAttempt `json:"attempts,omitempty"`
	Seasons     []StatusSeason  `json:"seasons,omitempty"`
	Episodes    []StatusEpisode `json:"episodes,omitempty"`
	// Rematch is the open Re-match moving files out of or into this item.
	Rematch *StatusRematch `json:"rematch,omitempty"`
	// Offload is the open Offload deleting this title's files, or the final
	// "Offloaded <date> · <bytes> freed" stage until it is dismissed.
	Offload *StatusOffload `json:"offload,omitempty"`
}

// statusFacts is everything one item's projection reads, once, and the
// indexes built over it once per poll.
type statusFacts struct {
	item       domain.MediaItem
	now        time.Time
	searches   []sqlite.SearchRecord // newest first, for the requested scope
	downloads  []sqlite.Download     // newest first; index() keeps the primary copy's
	pubFiles   map[int64][]sqlite.PublicationFile
	deliveries []sqlite.Delivery // newest first
	imported   time.Time         // the last "imported" history event

	byDownload map[int64][]int  // download id → delivery indexes
	byPath     map[string][]int // expected path → delivery indexes
	epDLs      map[int64][]int  // episode id → download indexes, newest first
	dlEps      map[int64]int    // download id → episodes it covers
	epFiles    map[int64][]domain.MediaFile
	files      []domain.MediaFile // primary-copy files
	rematch    *rematchFacts      // the open Re-match touching this item, if any
	offload    *OffloadStatus     // the open (or newest undismissed) Offload of this item
}

// ItemStatus projects where an item stands. For a series, season < 0 returns
// season summaries and season >= 0 that season's episodes.
func (s *Service) ItemStatus(ctx context.Context, itemID int64, season int) (ItemStatus, error) {
	f, err := s.statusFacts(ctx, itemID, season)
	if err != nil {
		return ItemStatus{}, err
	}
	return f.project(season), nil
}

func (s *Service) statusFacts(ctx context.Context, itemID int64, season int) (statusFacts, error) {
	f := statusFacts{now: time.Now()}
	var err error
	if f.item, err = s.db.GetMediaItemFull(ctx, itemID); err != nil {
		return f, err
	}
	// Searches: the movie's, or the one season asked for. A season summary
	// needs none (search never decides "in cinema").
	switch {
	case f.item.Kind != domain.KindSeries:
		f.searches, err = s.db.ItemSearchRecords(ctx, itemID, -1, sqlite.SearchRecordsKept)
	case season >= 0:
		f.searches, err = s.db.ItemSearchRecords(ctx, itemID, season, 5000)
	}
	if err != nil {
		return f, err
	}
	all, err := s.db.ItemDownloads(ctx, itemID, statusDownloadsRead)
	if err != nil {
		return f, err
	}
	f.downloads = all
	ids := make([]int64, 0, len(all))
	for _, d := range all {
		ids = append(ids, d.ID)
	}
	pubs, err := s.db.ListPublicationsForDownloads(ctx, ids)
	if err != nil {
		return f, err
	}
	var pubIDs []int64
	pubOf := map[int64]int64{}
	for dl, ps := range pubs {
		for _, p := range ps {
			pubIDs = append(pubIDs, p.ID)
			pubOf[p.ID] = dl
		}
	}
	files, err := s.db.ListPublicationFilesFor(ctx, pubIDs)
	if err != nil {
		return f, err
	}
	f.pubFiles = map[int64][]sqlite.PublicationFile{}
	for pub, fs := range files {
		f.pubFiles[pubOf[pub]] = append(f.pubFiles[pubOf[pub]], fs...)
	}
	if f.deliveries, err = s.db.ListItemDeliveries(ctx, itemID, statusDeliveriesRead); err != nil {
		return f, err
	}
	if f.imported, err = s.db.LastHistoryAt(ctx, itemID, "imported"); err != nil {
		return f, err
	}
	if f.rematch, err = s.rematchFacts(ctx, itemID, f.item); err != nil {
		return f, err
	}
	if f.offload, err = s.offloadFacts(ctx, itemID); err != nil {
		return f, err
	}
	return f, nil
}

// index builds the lookups the projection uses, once, over the primary
// copy's downloads and files.
func (f *statusFacts) index() {
	primary := f.downloads[:0:0]
	for _, d := range f.downloads {
		if d.CopyID == 0 {
			primary = append(primary, d)
		}
	}
	f.downloads = primary
	f.byDownload, f.byPath = map[int64][]int{}, map[string][]int{}
	for i, d := range f.deliveries {
		if d.DownloadID != 0 {
			f.byDownload[d.DownloadID] = append(f.byDownload[d.DownloadID], i)
		}
		for _, e := range d.ExpectedFiles {
			f.byPath[e.Path] = append(f.byPath[e.Path], i)
		}
	}
	f.files = nil
	for _, file := range f.item.Files {
		if file.CopyID == 0 {
			f.files = append(f.files, file)
		}
	}
	if f.item.Kind != domain.KindSeries {
		return
	}
	byNumber := map[[2]int]int64{}
	for _, sn := range f.item.Seasons {
		for _, e := range sn.Episodes {
			byNumber[[2]int{e.SeasonNumber, e.EpisodeNumber}] = e.ID
		}
	}
	f.epDLs, f.dlEps, f.epFiles = map[int64][]int{}, map[int64]int{}, map[int64][]domain.MediaFile{}
	for i, dl := range f.downloads {
		eps := downloadEpisodes(dl, byNumber)
		f.dlEps[dl.ID] = len(eps)
		for _, id := range eps {
			f.epDLs[id] = append(f.epDLs[id], i)
		}
	}
	for _, file := range f.files {
		for _, id := range file.EpisodeIDs {
			f.epFiles[id] = append(f.epFiles[id], file)
		}
	}
}

// downloadEpisodes maps a download to the episode ids it covers: the plan's
// reserved episodes, else its episode wantables.
func downloadEpisodes(dl sqlite.Download, byNumber map[[2]int]int64) []int64 {
	if len(dl.ReservedEpisodes) > 0 {
		return dl.ReservedEpisodes
	}
	var out []int64
	seen := map[int64]bool{}
	for _, w := range dl.WantableIDs {
		parts := strings.Split(w, ":")
		if len(parts) < 4 || parts[0] != "episode" {
			continue
		}
		s, _ := strconv.Atoi(parts[2])
		e, _ := strconv.Atoi(parts[3])
		if id, ok := byNumber[[2]int{s, e}]; ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (f statusFacts) project(season int) ItemStatus {
	f.index()
	out := ItemStatus{MediaItemID: f.item.ID, Kind: string(f.item.Kind), GeneratedAt: f.now.UnixMilli()}
	out.Rematch = f.rematchBlock()
	out.Offload = f.offloadBlock()
	if f.item.Kind != domain.KindSeries {
		cands := make([]*sqlite.Download, 0, len(f.downloads))
		for i := range f.downloads {
			cands = append(cands, &f.downloads[i])
		}
		dl, earlier := pickDownload(cands, len(f.files) > 0)
		var search *sqlite.SearchRecord
		if len(f.searches) > 0 {
			search = &f.searches[0]
		}
		t := f.track(dl, search, f.files)
		out.Stages, out.Headline, out.Cinema = t.stages, t.headline, t.cinema
		out.Download = f.statusDownload(dl)
		out.Active = dl != nil && inFlight(dl.State)
		out.Done = allDone(out.Stages)
		for _, a := range earlier {
			if len(out.Attempts) == statusAttemptsShown {
				break
			}
			out.Attempts = append(out.Attempts, attemptOf(*a))
		}
		out.Active, out.Done = out.Active || out.Rematch.running() || out.Offload.running(), out.Done && !out.Rematch.running() && !out.Offload.running()
		return out
	}
	for _, sn := range f.item.Seasons {
		if season >= 0 && sn.Number != season {
			continue
		}
		sum, eps := f.season(sn, season >= 0)
		out.Seasons = append(out.Seasons, sum)
		out.Episodes = append(out.Episodes, eps...)
		out.Active = out.Active || sum.Active
	}
	sort.Slice(out.Seasons, func(i, j int) bool { return out.Seasons[i].Season > out.Seasons[j].Season })
	if season >= 0 && len(out.Seasons) == 1 {
		out.Headline = StatusHeadline{State: seasonState(out.Seasons[0]), Title: seasonLabel(season), Detail: out.Seasons[0].Summary}
	} else {
		out.Headline = seriesHeadline(out.Seasons)
	}
	out.Done = out.Headline.State == StatusDone
	out.Active, out.Done = out.Active || out.Rematch.running() || out.Offload.running(), out.Done && !out.Rematch.running() && !out.Offload.running()
	return out
}

// pickDownload chooses the download a track follows: the newest, unless it
// ended in failure while the files it would have replaced are still there —
// then the files speak, and the failed grab is an earlier attempt.
func pickDownload(cands []*sqlite.Download, onDisk bool) (*sqlite.Download, []*sqlite.Download) {
	if len(cands) == 0 {
		return nil, nil
	}
	if onDisk && terminalFailure(cands[0]) {
		return nil, cands
	}
	return cands[0], cands[1:]
}

func terminalFailure(dl *sqlite.Download) bool {
	return dl.State == "failed" || dl.RunnerOutcome == ports.OutcomeFailed || dl.RunnerOutcome == ports.OutcomeCancelled
}

// ---- one track ---------------------------------------------------------

type track struct {
	stages   []StatusStage
	headline StatusHeadline
	cinema   *StatusCinema
}

// deliveriesFor is the sub-deliveries that speak for these files: the
// download's own, unless a later request (Ask cinema to confirm) named the
// files since; with no download, the newest that names a file.
func (f statusFacts) deliveriesFor(dl *sqlite.Download, files []domain.MediaFile) []sqlite.Delivery {
	var own []sqlite.Delivery
	newestOwn := -1
	if dl != nil {
		for _, i := range f.byDownload[dl.ID] {
			if newestOwn < 0 {
				newestOwn = i
			}
			own = append(own, f.deliveries[i])
		}
		if inFlight(dl.State) {
			return own
		}
	}
	best := -1
	for _, file := range files {
		if idx := f.byPath[file.Path]; len(idx) > 0 && (best < 0 || idx[0] < best) {
			best = idx[0]
		}
	}
	// Indexes are newest first: a smaller index is a newer row.
	if best >= 0 && (newestOwn < 0 || best < newestOwn) {
		return []sqlite.Delivery{f.deliveries[best]}
	}
	return own
}

func dlID(dl *sqlite.Download) int64 {
	if dl == nil {
		return 0
	}
	return dl.ID
}

func inFlight(state string) bool {
	switch state {
	case "planned", "grabbed", "submitting", "downloading", "held", "downloaded", "awaiting_import", "importing":
		return true
	}
	return false
}

func importedState(state string) bool { return state == "imported" || state == "notified" }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// postProcessing reports whether Runner's stage is past the fetch.
func postProcessing(stage string) bool {
	return stage != "" && stage != "downloading" && stage != "queued"
}

var stageWords = map[string]string{
	"par_rename": "renaming", "par_verify": "verifying", "par_repair": "repairing", "rar_rename": "naming archives",
	"unpack": "unpacking", "cleanup": "cleaning up", "move": "moving", "post_unpack_rename": "renaming",
	"publication_verify": "verifying the publication", "script": "running scripts",
}

func stageWord(stage string) string {
	if w, ok := stageWords[stage]; ok {
		return w
	}
	return strings.ReplaceAll(stage, "_", " ")
}

func (f statusFacts) track(dl *sqlite.Download, search *sqlite.SearchRecord, files []domain.MediaFile) track {
	st := make([]StatusStage, 6)
	for i, k := range []string{StageSearched, StageDownloading, StageProcessed, StageMoved, StageCinemaNotified, StageInCinema} {
		st[i] = StatusStage{Key: k, State: StatusTodo}
	}
	var t track
	onDisk := len(files) > 0
	var lastFile time.Time
	for _, file := range files {
		if file.AddedAt.After(lastFile) {
			lastFile = file.AddedAt
		}
	}
	placedAt := lastFile
	if !f.imported.IsZero() && f.imported.After(placedAt) {
		placedAt = f.imported
	}

	// Searched. Once something is on disk a later search (an upgrade that
	// found nothing, a skipped one) is detail, not a step backwards.
	searchActions := []string{ActionSearch, ActionInteractiveSearch}
	switch {
	case dl != nil:
		st[0] = StatusStage{Key: StageSearched, State: StatusDone, At: ms(dl.AddedAt), Detail: grabbedDetail(dl, f.searches)}
	case onDisk:
		d := "on disk"
		if search != nil && search.At.After(lastFile) {
			d += "; latest search " + searchDetail(*search)
		}
		st[0] = StatusStage{Key: StageSearched, State: StatusDone, At: ms(placedAt), Detail: d}
	case search != nil && search.Grabbed != "":
		st[0] = StatusStage{Key: StageSearched, State: StatusDone, At: ms(search.At), Detail: search.Trigger + " · grabbed " + search.Grabbed}
	case search != nil && search.Error != "" && search.Skipped == "":
		st[0] = StatusStage{Key: StageSearched, State: StatusFailed, At: ms(search.At), Detail: search.Trigger + " · " + search.Error, Actions: searchActions}
	case search != nil:
		st[0] = StatusStage{Key: StageSearched, State: StatusWaiting, At: ms(search.At), Detail: searchDetail(*search), Actions: searchActions}
	default:
		st[0].Actions = searchActions
	}

	if dl == nil {
		if onDisk {
			// Last known facts: the file is here; how it got here predates
			// the record or was an earlier grab.
			st[1] = StatusStage{Key: StageDownloading, State: StatusDone, Detail: "no current download"}
			st[2] = StatusStage{Key: StageProcessed, State: StatusDone}
			st[3] = StatusStage{Key: StageMoved, State: StatusDone, At: ms(placedAt), Detail: plural(len(files), "file") + " in the library"}
		}
	} else {
		f.downloadStages(st, dl)
	}

	// Cinema: only once the files are in the library.
	if st[3].State == StatusDone {
		t.cinema = cinemaStages(st, f.deliveriesFor(dl, files), files)
	}
	t.stages = st
	t.headline = f.headline(st, dl, search)
	return t
}

func grabbedDetail(dl *sqlite.Download, searches []sqlite.SearchRecord) string {
	from := "grabbed"
	if dl.Indexer != "" {
		from += " from " + dl.Indexer
	}
	for _, r := range searches {
		if r.DownloadID == dl.ID {
			return r.Trigger + " · " + from
		}
	}
	return from
}

func searchDetail(r sqlite.SearchRecord) string {
	if r.Skipped != "" {
		d := r.Trigger + " · not searched: " + strings.ReplaceAll(r.Skipped, "_", " ")
		if r.Error != "" {
			d += " (" + r.Error + ")"
		}
		return d
	}
	return fmt.Sprintf("%s · %d seen, %d accepted", r.Trigger, r.Seen, r.Accepted)
}

// failureStage is the one stage a failed download failed in: Runner's reason
// says which; with no reason, the stage Runner last reported.
func failureStage(dl *sqlite.Download) int {
	switch dl.RunnerOutcomeReason {
	case ports.ReasonPar, ports.ReasonUnpack, ports.ReasonScript, ports.ReasonPublish:
		return 2
	case "":
		if postProcessing(dl.RunnerStage) {
			return 2
		}
	}
	return 1
}

// importFailed reports a download that finished but whose import failed: a
// Runner terminal of completed, or a legacy row that fetched everything.
func importFailed(dl *sqlite.Download) bool {
	if dl.State != "failed" {
		return false
	}
	return dl.RunnerOutcome == ports.OutcomeCompleted || (dl.RunnerOutcome == "" && dl.Progress >= 1 && !postProcessing(dl.RunnerStage))
}

func (f statusFacts) downloadStages(st []StatusStage, dl *sqlite.Download) {
	pp := postProcessing(dl.RunnerStage)
	fetchedAt := dl.RunnerOutcomeAt
	if fetchedAt.IsZero() && pp {
		fetchedAt = dl.RunnerStageAt
	}
	fetched := humanBytes(dl.Size)
	if !fetchedAt.IsZero() && fetchedAt.After(dl.AddedAt) {
		fetched += " in " + fetchedAt.Sub(dl.AddedAt).Round(time.Second).String()
	}
	done := func(i int, at time.Time, detail string) {
		st[i] = StatusStage{Key: st[i].Key, State: StatusDone, At: ms(at), Detail: detail}
	}
	openActivity := []string{ActionOpenActivity}

	switch {
	case dl.State == "held":
		// A hold interrupts whichever stage Runner was in.
		i := 1
		if pp {
			i = 2
			done(1, fetchedAt, fetched)
		}
		d := "held by Runner"
		if dl.Error != "" {
			d += ": " + dl.Error
		}
		st[i] = StatusStage{Key: st[i].Key, State: StatusWaiting, At: ms(dl.StateEnteredAt), Detail: d, Actions: openActivity}
		return
	case importFailed(dl):
		done(1, fetchedAt, fetched)
		done(2, dl.RunnerOutcomeAt, "post-processing done")
		d := "import failed"
		if dl.Error != "" {
			d += ": " + dl.Error
		}
		st[3] = StatusStage{Key: StageMoved, State: StatusFailed, At: ms(dl.StateEnteredAt), Detail: d,
			Actions: []string{ActionRetryImport, ActionOpenActivity}}
		return
	case terminalFailure(dl):
		i := failureStage(dl)
		if i == 2 {
			done(1, fetchedAt, fetched)
		}
		st[i] = StatusStage{Key: st[i].Key, State: StatusFailed, At: ms(dl.RunnerOutcomeAt), Detail: failureDetail(dl),
			Actions: []string{ActionSearch, ActionOpenActivity}}
		return
	case dl.RunnerOutcome == ports.OutcomeCompleted || importedState(dl.State) ||
		dl.State == "downloaded" || dl.State == "awaiting_import" || dl.State == "importing":
		done(1, fetchedAt, fetched)
		done(2, dl.RunnerOutcomeAt, "post-processing done")
	case pp:
		done(1, fetchedAt, fetched)
		st[2] = StatusStage{Key: StageProcessed, State: StatusActive, At: ms(dl.RunnerStageAt), Detail: stageWord(dl.RunnerStage), Actions: openActivity}
		return
	case dl.State == "planned":
		st[1] = StatusStage{Key: StageDownloading, State: StatusActive, At: ms(dl.AddedAt), Detail: "grabbed, sending to runner",
			Actions: []string{ActionOpenActivity, ActionCancelDownload}}
		return
	case dl.State == "downloading" && dl.Progress > 0:
		d := fmt.Sprintf("%d%%", int(dl.Progress*100))
		if eta := f.etaSeconds(dl); eta > 0 {
			d += " · " + durationWords(eta) + " left"
		}
		st[1] = StatusStage{Key: StageDownloading, State: StatusActive, Detail: d, Actions: []string{ActionOpenActivity, ActionCancelDownload}}
		return
	default:
		st[1] = StatusStage{Key: StageDownloading, State: StatusActive, At: ms(dl.AddedAt), Detail: "queued in Runner",
			Actions: []string{ActionOpenActivity, ActionCancelDownload}}
		return
	}

	// Moved, for a download that finished.
	placed, total := 0, 0
	var lastPath string
	for _, pf := range f.pubFiles[dl.ID] {
		total++
		if pf.Outcome == "placed" {
			placed++
			lastPath = pf.LibraryPath
		}
	}
	switch {
	case importedState(dl.State):
		d := "imported"
		if placed > 0 {
			d = plural(placed, "file") + " placed"
		}
		st[3] = StatusStage{Key: StageMoved, State: StatusDone, At: ms(dl.StateEnteredAt), Detail: d}
	case dl.State == "importing":
		d := "placing"
		if total > 0 {
			d = fmt.Sprintf("placing · %d of %d files", placed, total)
		}
		if lastPath != "" {
			d += " · " + lastPath
		}
		st[3] = StatusStage{Key: StageMoved, State: StatusActive, At: ms(dl.StateEnteredAt), Detail: d, Actions: openActivity}
	default: // downloaded, awaiting_import, or a completed terminal not yet imported
		d := "waiting for import"
		if dl.Error != "" {
			d = dl.Error
		}
		st[3] = StatusStage{Key: StageMoved, State: StatusWaiting, At: ms(dl.StateEnteredAt), Detail: d,
			Actions: []string{ActionRetryImport, ActionOpenActivity}}
	}
}

func failureDetail(dl *sqlite.Download) string {
	d := dl.RunnerOutcome
	if d == "" {
		d = "failed"
	}
	if dl.RunnerOutcomeReason != "" {
		d += " (runner: " + dl.RunnerOutcomeReason + ")"
	}
	if dl.Error != "" {
		d += ": " + dl.Error
	}
	return d
}

// cinemaStages fills the last two stages from the sub-deliveries and
// returns the one a cinema action works on. A file counts as confirmed only
// by its own read-back; a delivery from a Cinema that reports no per-file
// results counts for the files it expected once it is visible.
func cinemaStages(st []StatusStage, dels []sqlite.Delivery, files []domain.MediaFile) *StatusCinema {
	confirm := []string{ActionConfirmCinema}
	if len(dels) == 0 {
		st[4] = StatusStage{Key: StageCinemaNotified, State: StatusTodo, Detail: "no scan request on record", Actions: confirm}
		st[5] = StatusStage{Key: StageInCinema, State: StatusTodo, Detail: "not confirmed", Actions: confirm}
		return nil
	}
	paths := map[string]bool{}
	for _, f := range files {
		paths[f.Path] = true
	}
	mine := func(p string) bool { return len(paths) == 0 || paths[p] }
	var placed, waiting, refused int
	var lastVisible int64
	var refusal string
	var itemID int64
	var warnings []string
	pick := dels[0]
	for _, d := range dels {
		if d.Status == "failed" && pick.Status != "failed" {
			pick = d
		}
		if len(d.FileResults) > 0 {
			warnings = append(warnings, deliveryWarnings(d)...)
			for _, r := range d.FileResults {
				// A removed path's entry is a warning, not a file of this item.
				if (r.State == "present" || r.State == "retained") || !mine(r.Path) {
					continue
				}
				switch {
				case r.State == "placed" && r.Matched:
					placed++
					at := d.VisibleAt
					if at == 0 {
						at = d.UpdatedAt // this file placed; another in the folder is not
					}
					lastVisible = max(lastVisible, at)
					if itemID == 0 {
						itemID = r.ItemID
					}
				case r.State == "skipped":
					refused++
					if refusal == "" {
						refusal = r.Reason
					}
				default:
					waiting++
				}
			}
			continue
		}
		named := false
		for _, e := range d.ExpectedFiles {
			named = named || mine(e.Path)
		}
		if !named {
			continue
		}
		if d.VisibleAt > 0 {
			placed++
			lastVisible = max(lastVisible, d.VisibleAt)
			if itemID == 0 && len(d.ItemIDs) > 0 {
				itemID = d.ItemIDs[0]
			}
		} else {
			waiting++
		}
	}
	c := &StatusCinema{DeliveryID: pick.ID, NotifierID: pick.NotifierID, Status: pick.Status, WaitingOn: pick.WaitingOn,
		VisibleAt: pick.VisibleAt, ItemIDs: pick.ItemIDs, Error: pick.LastError, Warnings: warnings}

	// Cinema notified.
	switch {
	case pick.Status == "failed":
		st[4] = StatusStage{Key: StageCinemaNotified, State: StatusFailed, At: pick.UpdatedAt, Detail: pick.LastError,
			Actions: []string{ActionResendCinema}}
	case pick.RemoteRequestID != "" || pick.Status == "ok":
		st[4] = StatusStage{Key: StageCinemaNotified, State: StatusDone, At: pick.CreatedAt, Detail: "scan accepted"}
	default:
		d := "sending"
		if pick.LastError != "" {
			d = "retrying: " + pick.LastError
		}
		st[4] = StatusStage{Key: StageCinemaNotified, State: StatusActive, At: pick.CreatedAt, Detail: d}
	}

	// In cinema.
	switch {
	case refused > 0:
		st[5] = StatusStage{Key: StageInCinema, State: StatusFailed, Detail: "cinema skipped " + plural(refused, "file") + ": " + refusal,
			Actions: []string{ActionResendCinema, ActionConfirmCinema}}
	case placed > 0 && waiting == 0:
		d := "confirmed"
		if itemID != 0 {
			d += " · cinema item " + strconv.FormatInt(itemID, 10)
		}
		if len(warnings) > 0 {
			d += "; warning: " + warnings[0]
			if len(warnings) > 1 {
				d += " (and " + strconv.Itoa(len(warnings)-1) + " more)"
			}
		}
		st[5] = StatusStage{Key: StageInCinema, State: StatusDone, At: lastVisible, Detail: d}
	case pick.Status == "failed":
		st[5] = StatusStage{Key: StageInCinema, State: StatusTodo, Detail: "not confirmed", Actions: confirm}
	case pick.Status == "ok" && pick.VisibleAt == 0:
		// Accepted by a Cinema that does not report results.
		st[5] = StatusStage{Key: StageInCinema, State: StatusTodo, Detail: pick.Result, Actions: confirm}
	default:
		d := "waiting"
		if pick.WaitingOn != "" {
			d = "behind " + strings.ReplaceAll(pick.WaitingOn, "_", " ")
		}
		st[5] = StatusStage{Key: StageInCinema, State: StatusWaiting, Detail: d}
	}
	return c
}

func allDone(st []StatusStage) bool {
	for _, s := range st {
		if s.State != StatusDone {
			return false
		}
	}
	return len(st) > 0
}

var stageNames = map[string]string{
	StageSearched: "Searched", StageDownloading: "Downloading", StageProcessed: "Processed",
	StageMoved: "Moved", StageCinemaNotified: "Cinema notified", StageInCinema: "In cinema",
}

// headline says the current stage in a sentence: the first stage that is
// not done, or "In cinema" when all are. Times go in At, never the text.
func (f statusFacts) headline(st []StatusStage, dl *sqlite.Download, search *sqlite.SearchRecord) StatusHeadline {
	if allDone(st) {
		return StatusHeadline{State: StatusDone, Title: "In cinema, confirmed", Detail: st[5].Detail, At: st[5].At}
	}
	for _, s := range st {
		if s.State == StatusDone {
			continue
		}
		h := StatusHeadline{State: s.State, Detail: s.Detail, At: s.At}
		switch {
		case s.Key == StageSearched && s.State == StatusTodo:
			h.Title = "Not searched yet"
		case s.Key == StageSearched && search != nil && search.Skipped != "":
			h.Title = "Search skipped"
		case s.Key == StageSearched && s.State == StatusFailed:
			h.Title = "Search failed"
		case s.Key == StageSearched:
			h.Title = "Waiting for a release"
			if search != nil {
				h.Detail = fmt.Sprintf("Searched (%s): %d releases found, %d accepted.", search.Trigger, search.Seen, search.Accepted)
				if search.TopRejection != "" {
					h.Detail += " Most refused: " + search.TopRejection + "."
				}
			}
		case s.State == StatusWaiting && dl != nil && dl.State == "held":
			h.Title = "Held by Runner"
		case s.Key == StageDownloading && s.State == StatusActive && dl != nil && dl.State == "planned":
			h.Title = "Grabbed, sending to runner"
			h.Detail = dl.ReleaseTitle
		case s.Key == StageDownloading && s.State == StatusActive && dl != nil && dl.Progress > 0:
			h.Title = "Downloading"
			if eta := f.etaSeconds(dl); eta > 0 {
				h.Title += ", " + durationWords(eta) + " left"
			}
			h.Detail = dl.ReleaseTitle
			if dl.Indexer != "" {
				h.Detail += " · " + dl.Indexer
			}
		case s.Key == StageDownloading && s.State == StatusActive:
			h.Title = "Queued in Runner"
			if dl != nil {
				h.Detail = "Grabbed " + dl.ReleaseTitle + ". Runner starts it when a slot frees."
			}
		case s.Key == StageProcessed && s.State == StatusActive:
			h.Title = "Runner is " + s.Detail
		case s.Key == StageMoved && s.State == StatusActive:
			h.Title = "Curator is moving it into the library"
		case s.Key == StageInCinema && s.State == StatusWaiting:
			h.Title = "In the library, cinema hasn't shown it yet"
		case s.Key == StageInCinema || s.Key == StageCinemaNotified:
			h.Title = "In the library, not confirmed in cinema"
		case s.State == StatusFailed:
			h.Title = stageNames[s.Key] + " failed"
		default:
			h.Title = stageNames[s.Key] + ": " + s.State
		}
		return h
	}
	return StatusHeadline{}
}

// rateFresh reports whether Runner's numbers on the row are current: the
// fetch is running and the row was updated recently. A stale ETA from a
// runner that stopped reporting is worse than none.
func (f statusFacts) rateFresh(dl *sqlite.Download) bool {
	return dl != nil && dl.State == "downloading" && !postProcessing(dl.RunnerStage) && f.now.Sub(dl.UpdatedAt) <= statusRateFresh
}

func (f statusFacts) etaSeconds(dl *sqlite.Download) int64 {
	if !f.rateFresh(dl) || dl.RateBps <= 0 || dl.RemainingBytes <= 0 {
		return 0
	}
	return (dl.RemainingBytes + dl.RateBps - 1) / dl.RateBps
}

func durationWords(s int64) string {
	if s < 60 {
		return fmt.Sprintf("%d s", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%d min %02d s", s/60, s%60)
	}
	return fmt.Sprintf("%d h %02d min", s/3600, (s%3600)/60)
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "size unknown"
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%d KB", n/1000)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (f statusFacts) statusDownload(dl *sqlite.Download) *StatusDownload {
	if dl == nil {
		return nil
	}
	out := &StatusDownload{ID: dl.ID, Release: dl.ReleaseTitle, Indexer: dl.Indexer, State: dl.State, Total: dl.Size,
		Progress: dl.Progress, Episodes: f.dlEps[dl.ID]}
	if f.rateFresh(dl) {
		out.RateBps, out.ETASeconds = dl.RateBps, f.etaSeconds(dl)
	}
	if dl.Size > 0 {
		out.Bytes = int64(dl.Progress * float64(dl.Size))
		if f.rateFresh(dl) && dl.RemainingBytes > 0 && dl.RemainingBytes <= dl.Size {
			out.Bytes = dl.Size - dl.RemainingBytes
		}
	}
	if dl.RunnerStage != "" {
		out.Stage, out.StageSince = dl.RunnerStage, ms(dl.RunnerStageAt)
	}
	return out
}

func attemptOf(dl sqlite.Download) StatusAttempt {
	a := StatusAttempt{DownloadID: dl.ID, Release: dl.ReleaseTitle, Indexer: dl.Indexer, At: ms(dl.AddedAt), Outcome: dl.State}
	if dl.RunnerOutcome != "" && dl.RunnerOutcome != ports.OutcomeCompleted {
		a.Outcome = dl.RunnerOutcome
	}
	if dl.RunnerOutcomeReason != "" {
		a.Reason = "runner: " + dl.RunnerOutcomeReason
	}
	if dl.Error != "" {
		a.Reason = strings.TrimPrefix(strings.Join([]string{a.Reason, dl.Error}, "; "), "; ")
	}
	return a
}

// ---- series ------------------------------------------------------------

// counted reports whether an episode is part of a season's summary: it is
// monitored and has aired, or there is a file for it anyway.
func counted(e domain.Episode, files int, today string) bool {
	return files > 0 || (e.Monitored && !e.Offloaded() && e.AirDate != "" && e.AirDate <= today)
}

func (f statusFacts) episode(e domain.Episode) (StatusEpisode, *sqlite.Download) {
	files := f.epFiles[e.ID]
	var cands []*sqlite.Download
	for _, i := range f.epDLs[e.ID] {
		cands = append(cands, &f.downloads[i])
	}
	dl, _ := pickDownload(cands, len(files) > 0)
	epWant := fmt.Sprintf("episode:%d:%d:%d", f.item.ID, e.SeasonNumber, e.EpisodeNumber)
	seasonWant := fmt.Sprintf("season:%d:%d", f.item.ID, e.SeasonNumber)
	var search *sqlite.SearchRecord
	for i := range f.searches {
		if w := f.searches[i].WantableID; w == epWant || w == seasonWant {
			search = &f.searches[i]
			break
		}
	}
	t := f.track(dl, search, files)
	out := StatusEpisode{EpisodeID: e.ID, Season: e.SeasonNumber, Episode: e.EpisodeNumber, Title: e.Title, WantableID: epWant,
		Stages: t.stages, Cinema: t.cinema, DownloadID: dlID(dl), InPack: dl != nil && f.dlEps[dl.ID] > 1}
	out.Summary, out.At = episodeSummary(t.stages, out.InPack)
	return out, dl
}

func episodeSummary(st []StatusStage, inPack bool) (string, int64) {
	if allDone(st) {
		return "in cinema", st[5].At
	}
	for _, s := range st {
		if s.State == StatusDone {
			continue
		}
		if s.State == StatusTodo && s.Key == StageSearched {
			return "not searched", 0
		}
		if inPack && (s.Key == StageDownloading || s.Key == StageProcessed) {
			return "in season pack · " + s.Detail, s.At
		}
		d := strings.ToLower(stageNames[s.Key])
		if s.Detail != "" {
			d += " · " + s.Detail
		}
		return d, s.At
	}
	return "", 0
}

// season summarizes one season; with episodes set it also returns each
// episode's strip.
func (f statusFacts) season(sn domain.Season, episodes bool) (StatusSeason, []StatusEpisode) {
	today := f.now.Format("2006-01-02")
	sum := StatusSeason{Season: sn.Number}
	var pack *sqlite.Download
	inPack := 0
	var eps []StatusEpisode
	for _, e := range sn.Episodes {
		ep, dl := f.episode(e)
		if episodes {
			eps = append(eps, ep)
		}
		if dl != nil && inFlight(dl.State) {
			sum.Active = true
			if ep.InPack {
				inPack++
				if pack == nil {
					pack = dl
				}
			}
		}
		if !counted(e, len(f.epFiles[e.ID]), today) {
			continue
		}
		sum.Episodes++
		if len(f.epFiles[e.ID]) > 0 {
			sum.OnDisk++
		}
		if allDone(ep.Stages) {
			sum.InCinema++
		}
	}
	sum.Summary = fmt.Sprintf("%d of %d in cinema", sum.InCinema, sum.Episodes)
	if inPack > 0 {
		sum.Summary += fmt.Sprintf(" · %d in one season pack", inPack)
	}
	sum.Download = f.statusDownload(pack)
	return sum, eps
}

func seasonLabel(n int) string {
	if n == 0 {
		return "Specials"
	}
	return fmt.Sprintf("Season %d", n)
}

func seasonState(s StatusSeason) string {
	switch {
	case s.Active:
		return StatusActive
	case s.Episodes > 0 && s.InCinema == s.Episodes:
		return StatusDone
	}
	return StatusWaiting
}

func seriesHeadline(seasons []StatusSeason) StatusHeadline {
	total, in, active := 0, 0, 0
	for _, s := range seasons {
		total += s.Episodes
		in += s.InCinema
		if s.Active {
			active++
		}
	}
	h := StatusHeadline{State: StatusWaiting, Title: fmt.Sprintf("%d of %d episodes in cinema", in, total)}
	switch {
	case active > 0:
		h.State, h.Detail = StatusActive, plural(active, "season")+" downloading or importing"
	case total > 0 && in == total:
		h.State = StatusDone
	}
	return h
}

// ---- Cancel download ----------------------------------------------------

// CancelAndBlocklist is the item page's Cancel download: cancel the job in
// its client exactly as Activity's Remove does, then blocklist the release
// (by its indexer identity when known, else title and indexer) so the next
// backlog pass or RSS sync does not grab the same release straight back.
// Activity's Remove stays a removal, not a verdict on the release.
func (s *Service) CancelAndBlocklist(ctx context.Context, id int64) error {
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	if err := s.RemoveDownload(ctx, id, true); err != nil {
		return err
	}
	const reason = "cancelled from the item page"
	if dl.SourceGUID != "" && dl.SourceIndexerID > 0 {
		return s.db.AddReleaseIdentityBlocklist(ctx, dl.MediaItemID, dl.SourceIndexerID, dl.SourceGUID, reason)
	}
	return s.db.AddBlocklist(ctx, dl.MediaItemID, dl.ReleaseTitle, dl.Indexer, reason)
}

// ---- Ask cinema to confirm ----------------------------------------------

// ErrNothingToConfirm: the item has no files, or no notifier reports to
// Cinema.
var ErrNothingToConfirm = errors.New("nothing to confirm")

// ConfirmInCinema asks Cinema to scan the item's folders (or one episode's)
// with its current files as the expected files, through the same outbox an
// import writes, so the read-back confirms (or names what is missing). The
// key is stable per item or episode: asking again refreshes the same
// sub-delivery and advances its attempt instead of adding rows. It returns
// how many directory requests were queued.
func (s *Service) ConfirmInCinema(ctx context.Context, itemID, episodeID int64) (int, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return 0, err
	}
	e := ImportCompleted{MediaItemID: item.ID, Release: item.Title, MediaItemKind: string(item.Kind), Title: item.Title,
		TmdbID: item.IDs.TMDB, ImdbID: item.IDs.IMDB, Expected: map[string][]ports.ExpectedFile{}}
	if item.Kind == domain.KindBook {
		e.BookTitle, e.BookAuthor, e.BookWorkID = curatorBookText(item.Title), curatorBookText(item.Author), curatorBookWorkID(item)
	}
	generation := fmt.Sprintf("confirm-%d", itemID)
	if episodeID != 0 {
		generation = fmt.Sprintf("confirm-%d-e%d", itemID, episodeID)
	}
	for _, file := range item.Files {
		if episodeID != 0 && !containsID(file.EpisodeIDs, episodeID) {
			continue
		}
		fi, err := os.Stat(file.Path)
		if err != nil {
			continue // a file curator cannot see is not one to ask Cinema about
		}
		dir := filepath.Dir(file.Path)
		if _, ok := e.Expected[dir]; !ok {
			e.Dirs = append(e.Dirs, dir)
		}
		e.Paths = append(e.Paths, file.Path)
		e.Files++
		e.Expected[dir] = append(e.Expected[dir], ports.ExpectedFile{Path: file.Path, Bytes: fi.Size(), MtimeNs: fi.ModTime().UnixNano()})
	}
	if e.Files == 0 {
		return 0, fmt.Errorf("%w: no readable files in the library for this item", ErrNothingToConfirm)
	}
	notifiers, err := s.db.ListNotifiers(ctx)
	if err != nil {
		return 0, err
	}
	rows, _, err := ImportOutbox(notifiers, e, generation)
	if err != nil {
		return 0, err
	}
	// Only the sub-deliveries: a chat notifier is not told "Import completed"
	// for an import that did not happen.
	var subs []sqlite.OutboxWrite
	for _, r := range rows {
		if r.IdempotencyKey != "" {
			subs = append(subs, r)
		}
	}
	if len(subs) == 0 {
		return 0, fmt.Errorf("%w: no enabled cinema notifier with import notifications on", ErrNothingToConfirm)
	}
	if err := s.db.EnqueueDeliveries(ctx, subs); err != nil {
		return 0, err
	}
	return len(subs), nil
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
