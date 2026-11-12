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
// movie, or each episode of a series, stands from search to Cinema. Like
// QueuePage and ImportActivity it reads what the pipeline already records —
// search records, download rows, publications, sub-deliveries — and decides
// nothing.

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
}

// StatusHeadline is the one line at the top of the panel.
type StatusHeadline struct {
	State  string `json:"state"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// StatusEpisode is one episode's strip.
type StatusEpisode struct {
	EpisodeID int64         `json:"episodeId"`
	Season    int           `json:"season"`
	Episode   int           `json:"episode"`
	Title     string        `json:"title,omitempty"`
	Summary   string        `json:"summary"`
	InPack    bool          `json:"inPack"`
	Stages    []StatusStage `json:"stages"`
}

// StatusSeason is one season's summary; its season-pack download is shown
// here once, not on every episode.
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
	Done        bool            `json:"done"`   // every stage done: collapse to one line
	Headline    StatusHeadline  `json:"headline"`
	Stages      []StatusStage   `json:"stages,omitempty"`
	Download    *StatusDownload `json:"download,omitempty"`
	Cinema      *StatusCinema   `json:"cinema,omitempty"`
	Attempts    []StatusAttempt `json:"attempts,omitempty"`
	Seasons     []StatusSeason  `json:"seasons,omitempty"`
	Episodes    []StatusEpisode `json:"episodes,omitempty"`
}

// statusFacts is everything one item's projection reads, once.
type statusFacts struct {
	item       domain.MediaItem
	searches   []sqlite.SearchRecord
	downloads  []sqlite.Download // newest first
	pubFiles   map[int64][]sqlite.PublicationFile
	deliveries []sqlite.Delivery // newest first
	imported   time.Time         // last "imported" history event
}

// ItemStatus projects where an item stands. For a series, season < 0 returns
// season summaries and season >= 0 that season's episodes.
func (s *Service) ItemStatus(ctx context.Context, itemID int64, season int) (ItemStatus, error) {
	f, err := s.statusFacts(ctx, itemID)
	if err != nil {
		return ItemStatus{}, err
	}
	out := ItemStatus{MediaItemID: itemID, Kind: string(f.item.Kind), GeneratedAt: time.Now().UnixMilli()}
	if f.item.Kind != domain.KindSeries {
		var dl *sqlite.Download
		if len(f.downloads) > 0 {
			dl = &f.downloads[0]
		}
		t := f.track(dl, f.searchFor(func(r sqlite.SearchRecord) bool { return r.Season < 0 }), f.item.Files, f.deliveriesFor(dl, f.item.Files))
		out.Stages, out.Headline, out.Cinema = t.stages, t.headline, t.cinema
		out.Download = statusDownload(dl, f.downloadEpisodes(dl))
		out.Active = dl != nil && inFlight(dl.State)
		out.Done = allDone(out.Stages)
		for i := 1; i < len(f.downloads) && len(out.Attempts) < 5; i++ {
			out.Attempts = append(out.Attempts, attemptOf(f.downloads[i]))
		}
		return out, nil
	}
	if season < 0 {
		out.Seasons = f.seasons()
		for _, s := range out.Seasons {
			out.Active = out.Active || s.Active
		}
		out.Headline = seriesHeadline(out.Seasons)
		out.Done = len(out.Seasons) > 0
		for _, s := range out.Seasons {
			out.Done = out.Done && s.InCinema == s.Episodes
		}
		return out, nil
	}
	for _, sn := range f.item.Seasons {
		if sn.Number != season {
			continue
		}
		for _, e := range sn.Episodes {
			out.Episodes = append(out.Episodes, f.episode(e))
		}
	}
	for _, sum := range f.seasons() {
		if sum.Season == season {
			out.Seasons = []StatusSeason{sum}
			out.Active = sum.Active
			out.Headline = StatusHeadline{State: seasonState(sum), Title: seasonLabel(season), Detail: sum.Summary}
		}
	}
	return out, nil
}

func (s *Service) statusFacts(ctx context.Context, itemID int64) (statusFacts, error) {
	var f statusFacts
	var err error
	if f.item, err = s.db.GetMediaItemFull(ctx, itemID); err != nil {
		return f, err
	}
	if f.searches, err = s.db.ItemSearchRecords(ctx, itemID, 200); err != nil {
		return f, err
	}
	if f.downloads, err = s.db.ItemDownloads(ctx, itemID); err != nil {
		return f, err
	}
	ids := make([]int64, 0, len(f.downloads))
	for _, d := range f.downloads {
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
	if f.deliveries, err = s.db.ListItemDeliveries(ctx, itemID, 500); err != nil {
		return f, err
	}
	history, err := s.db.HistoryPage(ctx, itemID, 50, 0)
	if err != nil {
		return f, err
	}
	for _, h := range history {
		if h.Type == "imported" {
			f.imported = h.At
			break
		}
	}
	return f, nil
}

// ---- one track ---------------------------------------------------------

type track struct {
	stages   []StatusStage
	headline StatusHeadline
	cinema   *StatusCinema
}

func (f statusFacts) searchFor(keep func(sqlite.SearchRecord) bool) *sqlite.SearchRecord {
	for i := range f.searches {
		if keep(f.searches[i]) {
			return &f.searches[i]
		}
	}
	return nil
}

// deliveriesFor is the sub-deliveries that speak for these files: the
// download's own, unless a later request (Ask cinema to confirm) named the
// files since; with no download, the newest that names a file.
func (f statusFacts) deliveriesFor(dl *sqlite.Download, files []domain.MediaFile) []sqlite.Delivery {
	var own []sqlite.Delivery
	if dl != nil {
		for _, d := range f.deliveries {
			if d.DownloadID == dl.ID {
				own = append(own, d)
			}
		}
		if inFlight(dl.State) {
			return own
		}
	}
	paths := map[string]bool{}
	for _, file := range files {
		paths[file.Path] = true
	}
	for _, d := range f.deliveries { // newest first
		if dl != nil && d.DownloadID == dl.ID {
			break // the download's own are the newest word
		}
		for _, e := range d.ExpectedFiles {
			if paths[e.Path] {
				return []sqlite.Delivery{d}
			}
		}
	}
	return own
}

func inFlight(state string) bool {
	switch state {
	case "planned", "grabbed", "submitting", "downloading", "held", "downloaded", "awaiting_import", "importing":
		return true
	}
	return false
}

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

func (f statusFacts) track(dl *sqlite.Download, search *sqlite.SearchRecord, files []domain.MediaFile, dels []sqlite.Delivery) track {
	st := make([]StatusStage, 6)
	for i, k := range []string{StageSearched, StageDownloading, StageProcessed, StageMoved, StageCinemaNotified, StageInCinema} {
		st[i] = StatusStage{Key: k, State: StatusTodo}
	}
	var t track
	onDisk := len(files) > 0
	lastFile := time.Time{}
	for _, file := range files {
		if file.AddedAt.After(lastFile) {
			lastFile = file.AddedAt
		}
	}

	// Searched.
	switch {
	case dl != nil:
		st[0] = StatusStage{Key: StageSearched, State: StatusDone, At: ms(dl.AddedAt), Detail: grabbedDetail(dl, f.searches)}
	case search != nil && search.Grabbed != "":
		st[0] = StatusStage{Key: StageSearched, State: StatusDone, At: ms(search.At), Detail: search.Trigger + " · grabbed " + search.Grabbed}
	case search != nil && search.Error != "" && search.Skipped == "":
		st[0] = StatusStage{Key: StageSearched, State: StatusFailed, At: ms(search.At), Detail: search.Trigger + " · " + search.Error,
			Actions: []string{ActionSearch, ActionInteractiveSearch}}
	case search != nil && (!onDisk || search.At.After(lastFile)):
		st[0] = StatusStage{Key: StageSearched, State: StatusWaiting, At: ms(search.At), Detail: searchDetail(*search),
			Actions: []string{ActionSearch, ActionInteractiveSearch}}
	case onDisk:
		st[0] = StatusStage{Key: StageSearched, State: StatusDone, Detail: "before search records were kept"}
	default:
		st[0].Actions = []string{ActionSearch, ActionInteractiveSearch}
	}

	if dl == nil {
		if onDisk {
			// Last known facts: the file is here; how it got here predates
			// the record.
			at := lastFile
			if !f.imported.IsZero() {
				at = f.imported
			}
			st[1] = StatusStage{Key: StageDownloading, State: StatusDone, Detail: "no download on record"}
			st[2] = StatusStage{Key: StageProcessed, State: StatusDone}
			st[3] = StatusStage{Key: StageMoved, State: StatusDone, At: ms(at), Detail: plural(len(files), "file") + " in the library"}
		}
	} else {
		f.downloadStages(st, dl)
	}

	// Cinema: only once the files are in the library.
	if st[3].State == StatusDone {
		t.cinema = cinemaStages(st, dels, files)
	}
	t.stages = st
	t.headline = headline(st, dl, search)
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

func (f statusFacts) downloadStages(st []StatusStage, dl *sqlite.Download) {
	pp := postProcessing(dl.RunnerStage)
	finished := dl.RunnerOutcome == ports.OutcomeCompleted || dl.State == "downloaded" || dl.State == "awaiting_import" || dl.State == "importing" || dl.State == "imported"
	failed := dl.State == "failed" || dl.State == "removed" || dl.RunnerOutcome == ports.OutcomeFailed || dl.RunnerOutcome == ports.OutcomeCancelled
	doneAt := dl.RunnerOutcomeAt
	if doneAt.IsZero() && pp {
		doneAt = dl.RunnerStageAt
	}
	took := ""
	if !doneAt.IsZero() && doneAt.After(dl.AddedAt) {
		took = " in " + doneAt.Sub(dl.AddedAt).Round(time.Second).String()
	}
	size := humanBytes(dl.Size)

	// Downloading.
	switch {
	case failed && !finished && !pp:
		st[1] = StatusStage{Key: StageDownloading, State: StatusFailed, At: ms(dl.RunnerOutcomeAt), Detail: failureDetail(dl),
			Actions: []string{ActionSearch, ActionOpenActivity}}
	case finished || pp:
		st[1] = StatusStage{Key: StageDownloading, State: StatusDone, At: ms(doneAt), Detail: size + took}
	case dl.State == "held":
		st[1] = StatusStage{Key: StageDownloading, State: StatusWaiting, Detail: "held by Runner: " + dl.Error,
			Actions: []string{ActionOpenActivity}}
	case dl.State == "downloading" && dl.Progress > 0:
		d := fmt.Sprintf("%d%%", int(dl.Progress*100))
		if eta := etaSeconds(dl); eta > 0 {
			d += " · " + durationWords(eta) + " left"
		}
		st[1] = StatusStage{Key: StageDownloading, State: StatusActive, Detail: d, Actions: []string{ActionOpenActivity, ActionCancelDownload}}
	default:
		st[1] = StatusStage{Key: StageDownloading, State: StatusActive, Detail: "queued in Runner", Actions: []string{ActionOpenActivity, ActionCancelDownload}}
	}

	// Processed.
	switch {
	case failed && (pp || dl.RunnerOutcomeReason == ports.ReasonPar || dl.RunnerOutcomeReason == ports.ReasonUnpack || dl.RunnerOutcomeReason == ports.ReasonScript):
		st[2] = StatusStage{Key: StageProcessed, State: StatusFailed, At: ms(dl.RunnerOutcomeAt), Detail: failureDetail(dl),
			Actions: []string{ActionSearch, ActionOpenActivity}}
	case finished:
		st[2] = StatusStage{Key: StageProcessed, State: StatusDone, At: ms(dl.RunnerOutcomeAt), Detail: "verified, unpacked"}
	case pp:
		st[2] = StatusStage{Key: StageProcessed, State: StatusActive, At: ms(dl.RunnerStageAt), Detail: stageWord(dl.RunnerStage),
			Actions: []string{ActionOpenActivity}}
	}

	// Moved.
	files := f.pubFiles[dl.ID]
	placed := 0
	var lastPath string
	for _, pf := range files {
		if pf.Outcome == "placed" {
			placed++
			lastPath = pf.LibraryPath
		}
	}
	switch dl.State {
	case "imported":
		d := plural(max(placed, 1), "file") + " placed"
		if placed == 0 {
			d = "imported"
		}
		st[3] = StatusStage{Key: StageMoved, State: StatusDone, At: ms(dl.StateEnteredAt), Detail: d}
	case "importing":
		d := "placing"
		if len(files) > 0 {
			d = fmt.Sprintf("placing · %d of %d files", placed, len(files))
		}
		if lastPath != "" {
			d += " · " + lastPath
		}
		st[3] = StatusStage{Key: StageMoved, State: StatusActive, At: ms(dl.StateEnteredAt), Detail: d, Actions: []string{ActionOpenActivity}}
	case "downloaded", "awaiting_import":
		d := "waiting for import"
		if dl.Error != "" {
			d = dl.Error
		}
		st[3] = StatusStage{Key: StageMoved, State: StatusWaiting, At: ms(dl.StateEnteredAt), Detail: d,
			Actions: []string{ActionRetryImport, ActionOpenActivity}}
	case "failed":
		if finished {
			st[3] = StatusStage{Key: StageMoved, State: StatusFailed, At: ms(dl.StateEnteredAt), Detail: "import failed: " + dl.Error,
				Actions: []string{ActionRetryImport, ActionOpenActivity}}
		}
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
// returns the one a cinema action works on. For episode tracks only the
// files' own read-back counts.
func cinemaStages(st []StatusStage, dels []sqlite.Delivery, files []domain.MediaFile) *StatusCinema {
	if len(dels) == 0 {
		st[4] = StatusStage{Key: StageCinemaNotified, State: StatusTodo, Detail: "no scan request on record", Actions: []string{ActionConfirmCinema}}
		st[5] = StatusStage{Key: StageInCinema, State: StatusTodo, Detail: "not confirmed", Actions: []string{ActionConfirmCinema}}
		return nil
	}
	paths := map[string]bool{}
	for _, f := range files {
		paths[f.Path] = true
	}
	// Per-file results first: they are what makes an episode's state its own.
	var placed, waiting, refused int
	var lastVisible int64
	var refusal string
	pick := dels[0]
	for _, d := range dels {
		if d.Status == "failed" && pick.Status != "failed" {
			pick = d
		}
		if d.VisibleAt > lastVisible {
			lastVisible = d.VisibleAt
		}
		for _, r := range d.FileResults {
			if len(paths) > 0 && !paths[r.Path] {
				continue
			}
			switch {
			case r.State == "placed" && r.Matched:
				placed++
			case r.State == "skipped":
				refused++
				if refusal == "" {
					refusal = r.Reason
				}
			default:
				waiting++
			}
		}
	}
	c := &StatusCinema{DeliveryID: pick.ID, NotifierID: pick.NotifierID, Status: pick.Status, WaitingOn: pick.WaitingOn,
		VisibleAt: pick.VisibleAt, ItemIDs: pick.ItemIDs, Error: pick.LastError}

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
	allVisible := true
	for _, d := range dels {
		allVisible = allVisible && d.VisibleAt > 0
	}
	switch {
	case refused > 0:
		st[5] = StatusStage{Key: StageInCinema, State: StatusFailed, Detail: "cinema skipped " + plural(refused, "file") + ": " + refusal,
			Actions: []string{ActionResendCinema, ActionConfirmCinema}}
	case placed > 0 && waiting == 0, placed == 0 && waiting == 0 && allVisible:
		d := "confirmed"
		if ids := c.ItemIDs; len(ids) > 0 {
			d += " · cinema item " + strconv.FormatInt(ids[0], 10)
		}
		st[5] = StatusStage{Key: StageInCinema, State: StatusDone, At: lastVisible, Detail: d}
	case pick.Status == "failed":
		st[5] = StatusStage{Key: StageInCinema, State: StatusTodo, Detail: "not confirmed", Actions: []string{ActionConfirmCinema}}
	case pick.Status == "ok" && pick.VisibleAt == 0:
		// Accepted by a Cinema that does not report results.
		st[5] = StatusStage{Key: StageInCinema, State: StatusTodo, Detail: pick.Result, Actions: []string{ActionConfirmCinema}}
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
// not done, or "In cinema" when all are.
func headline(st []StatusStage, dl *sqlite.Download, search *sqlite.SearchRecord) StatusHeadline {
	if allDone(st) {
		return StatusHeadline{State: StatusDone, Title: "In cinema, confirmed " + clock(st[5].At), Detail: st[5].Detail}
	}
	for _, s := range st {
		if s.State == StatusDone {
			continue
		}
		h := StatusHeadline{State: s.State, Detail: s.Detail}
		switch {
		case s.Key == StageSearched && s.State == StatusTodo:
			h.Title = "Not searched yet"
		case s.Key == StageSearched && search != nil && search.Skipped != "":
			h.Title = "Search skipped"
		case s.Key == StageSearched:
			h.Title = "Waiting for a release"
			if search != nil {
				h.Detail = fmt.Sprintf("Searched at %s (%s). %d releases found, %d accepted.", clock(ms(search.At)), search.Trigger, search.Seen, search.Accepted)
				if search.TopRejection != "" {
					h.Detail += " Most refused: " + search.TopRejection + "."
				}
			}
		case s.Key == StageDownloading && s.State == StatusActive && dl != nil && dl.Progress > 0:
			h.Title = "Downloading"
			if eta := etaSeconds(dl); eta > 0 {
				h.Title += ", " + durationWords(eta) + " left"
			}
			h.Detail = dl.ReleaseTitle
			if dl.Indexer != "" {
				h.Detail += " · " + dl.Indexer
			}
		case s.Key == StageDownloading && s.State == StatusActive:
			h.Title = "Queued in Runner"
			if dl != nil {
				h.Detail = "Grabbed " + dl.ReleaseTitle + " at " + clock(ms(dl.AddedAt)) + ". Runner starts it when a slot frees."
			}
		case s.Key == StageProcessed && s.State == StatusActive:
			h.Title = "Runner is " + s.Detail
			h.Detail = "Since " + clock(s.At) + "."
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

func clock(at int64) string {
	if at == 0 {
		return ""
	}
	t := time.UnixMilli(at)
	if time.Since(t) > 20*time.Hour {
		return t.Format("Jan 2")
	}
	return t.Format("15:04")
}

func etaSeconds(dl *sqlite.Download) int64 {
	if dl == nil || dl.RateBps <= 0 || dl.RemainingBytes <= 0 {
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

func statusDownload(dl *sqlite.Download, episodes int) *StatusDownload {
	if dl == nil {
		return nil
	}
	out := &StatusDownload{ID: dl.ID, Release: dl.ReleaseTitle, Indexer: dl.Indexer, State: dl.State, Total: dl.Size,
		Progress: dl.Progress, RateBps: dl.RateBps, ETASeconds: etaSeconds(dl), Episodes: episodes}
	if dl.Size > 0 {
		out.Bytes = int64(dl.Progress * float64(dl.Size))
		if dl.RemainingBytes > 0 && dl.RemainingBytes <= dl.Size {
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

// downloadEpisodes is how many episodes a download covers.
func (f statusFacts) downloadEpisodes(dl *sqlite.Download) int {
	if dl == nil {
		return 0
	}
	return len(f.episodesOf(*dl))
}

// episodesOf maps a download to the episode ids it covers: the plan's
// reserved episodes, else its episode wantables.
func (f statusFacts) episodesOf(dl sqlite.Download) map[int64]bool {
	out := map[int64]bool{}
	for _, id := range dl.ReservedEpisodes {
		out[id] = true
	}
	if len(out) > 0 {
		return out
	}
	byNumber := map[[2]int]int64{}
	for _, sn := range f.item.Seasons {
		for _, e := range sn.Episodes {
			byNumber[[2]int{e.SeasonNumber, e.EpisodeNumber}] = e.ID
		}
	}
	for _, w := range dl.WantableIDs {
		parts := strings.Split(w, ":")
		if len(parts) < 4 || parts[0] != "episode" {
			continue
		}
		s, _ := strconv.Atoi(parts[2])
		e, _ := strconv.Atoi(parts[3])
		if id, ok := byNumber[[2]int{s, e}]; ok {
			out[id] = true
		}
	}
	return out
}

func (f statusFacts) episodeDownload(epID int64) *sqlite.Download {
	for i := range f.downloads {
		if f.episodesOf(f.downloads[i])[epID] {
			return &f.downloads[i]
		}
	}
	return nil
}

func (f statusFacts) episodeFiles(epID int64) []domain.MediaFile {
	var out []domain.MediaFile
	for _, file := range f.item.Files {
		for _, id := range file.EpisodeIDs {
			if id == epID {
				out = append(out, file)
				break
			}
		}
	}
	return out
}

func (f statusFacts) episode(e domain.Episode) StatusEpisode {
	dl := f.episodeDownload(e.ID)
	files := f.episodeFiles(e.ID)
	epWant := fmt.Sprintf("episode:%d:%d:%d", f.item.ID, e.SeasonNumber, e.EpisodeNumber)
	seasonWant := fmt.Sprintf("season:%d:%d", f.item.ID, e.SeasonNumber)
	search := f.searchFor(func(r sqlite.SearchRecord) bool {
		w := strings.SplitN(r.WantableID, ":c", 2)[0]
		return w == epWant || w == seasonWant
	})
	// A season pack's delivery covers many episodes: only this episode's
	// files' own read-back speaks for it.
	dels := f.deliveriesFor(dl, files)
	t := f.track(dl, search, files, dels)
	out := StatusEpisode{EpisodeID: e.ID, Season: e.SeasonNumber, Episode: e.EpisodeNumber, Title: e.Title, Stages: t.stages,
		InPack: dl != nil && len(f.episodesOf(*dl)) > 1}
	out.Summary = episodeSummary(t.stages, out.InPack)
	return out
}

func episodeSummary(st []StatusStage, inPack bool) string {
	if allDone(st) {
		return "in cinema · " + clock(st[5].At)
	}
	for _, s := range st {
		if s.State == StatusDone {
			continue
		}
		d := stageNames[s.Key]
		if s.Detail != "" {
			d = strings.ToLower(d) + " · " + s.Detail
		}
		if s.State == StatusTodo && s.Key == StageSearched {
			d = "not searched"
		}
		if inPack && (s.Key == StageDownloading || s.Key == StageProcessed) {
			d = "in season pack · " + s.Detail
		}
		return d
	}
	return ""
}

func (f statusFacts) seasons() []StatusSeason {
	var out []StatusSeason
	for _, sn := range f.item.Seasons {
		sum := StatusSeason{Season: sn.Number, Episodes: len(sn.Episodes)}
		packs := map[int64]bool{}
		var pack *sqlite.Download
		inPack := 0
		for _, e := range sn.Episodes {
			ep := f.episode(e)
			if allDone(ep.Stages) {
				sum.InCinema++
			}
			if e.HasFile {
				sum.OnDisk++
			}
			dl := f.episodeDownload(e.ID)
			if dl != nil && inFlight(dl.State) {
				sum.Active = true
				if ep.InPack {
					inPack++
					if !packs[dl.ID] {
						packs[dl.ID] = true
						if pack == nil {
							pack = dl
						}
					}
				}
			}
		}
		sum.Summary = fmt.Sprintf("%d of %d in cinema", sum.InCinema, sum.Episodes)
		if inPack > 0 {
			sum.Summary += fmt.Sprintf(" · %d in one season pack", inPack)
		}
		sum.Download = statusDownload(pack, f.downloadEpisodes(pack))
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Season > out[j].Season })
	return out
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
	total, in := 0, 0
	active := 0
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

// ---- Ask cinema to confirm ----------------------------------------------

// ErrNothingToConfirm: the item has no files, or no notifier reports to
// Cinema.
var ErrNothingToConfirm = errors.New("nothing to confirm")

// ConfirmInCinema asks Cinema to scan the item's folders with its current
// files as the expected files, through the same outbox an import writes, so
// the read-back confirms (or names what is missing). It returns how many
// directory requests were queued.
func (s *Service) ConfirmInCinema(ctx context.Context, itemID int64) (int, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return 0, err
	}
	e := ImportCompleted{MediaItemID: item.ID, Release: item.Title, MediaItemKind: string(item.Kind), Title: item.Title,
		TmdbID: item.IDs.TMDB, ImdbID: item.IDs.IMDB, Expected: map[string][]ports.ExpectedFile{}}
	if item.Kind == domain.KindBook {
		e.BookTitle, e.BookAuthor, e.BookWorkID = curatorBookText(item.Title), curatorBookText(item.Author), curatorBookWorkID(item)
	}
	for _, file := range item.Files {
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
	rows, _, err := ImportOutbox(notifiers, e, legacyGeneration())
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
