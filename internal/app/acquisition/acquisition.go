// Package acquisition is the Phase 2 orchestrator: interactive search
// (fan out → parse → match → decide, rejection reasons attached), grabbing
// to the right download client, queue tracking, and import (blueprint §5.1).
package acquisition

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monarr-media/monarr/internal/app/health"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/decision"
	"github.com/monarr-media/monarr/internal/domain/format"
	"github.com/monarr-media/monarr/internal/domain/matcher"
	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// Factories let cmd/monarr wire real adapters while tests inject fakes —
// the app layer depends on ports only.
type (
	IndexerFactory func(ports.IndexerConfig) ports.Indexer
	ClientFactory  func(ports.ClientConfig) ports.DownloadClient
)

// Errors surfaced to the API layer.
var (
	ErrNoIndexers = errors.New("no enabled indexers configured")
	ErrNoClient   = errors.New("no enabled download client for this protocol")
	ErrNotFound   = sqlite.ErrNotFound
)

// Events.
type (
	// ReleaseGrabbed is published after a successful grab.
	ReleaseGrabbed struct {
		MediaItemID int64  `json:"mediaItemId"`
		Title       string `json:"title"`
		Indexer     string `json:"indexer"`
		Protocol    string `json:"protocol"`
	}
	// ImportCompleted is published after files land in the library.
	//
	// It carries the paths and the ids because of who reads it. A
	// notification only needs a sentence; a media server needs to know
	// exactly what appeared and what it is. Telling plurx "something
	// changed, go and look" costs a full library sweep and can still match
	// the wrong film from a folder name — telling it "this directory is
	// tmdb 949" costs one folder and cannot be mismatched. None of it can
	// be reconstructed downstream: the bus event is the only source the
	// notifier gets, so what is not on here cannot be sent (plan §5.4).
	ImportCompleted struct {
		MediaItemID int64  `json:"mediaItemId"`
		Release     string `json:"release"`
		Files       int    `json:"files"`
		Upgrade     bool   `json:"upgrade"`
		// Paths are every placed file, absolute, as MONARR sees them. A
		// consumer on another host may need a path mapping — the same
		// caveat the download clients already carry.
		Paths []string `json:"paths,omitempty"`
		// Dirs are the unique parent directories of Paths, in first-seen
		// order: what a media server is actually asked to index. A season
		// pack is one directory and a dozen files, and a dozen requests
		// saying the same thing is a dozen chances for one to fail.
		Dirs []string `json:"dirs,omitempty"`
		// MediaItemKind is movie | series | book. Not a bool: "not a movie"
		// is three different things downstream, and books have no plurx
		// library kind at all (plan §10.7).
		MediaItemKind string `json:"mediaItemKind,omitempty"`
		Title         string `json:"title,omitempty"`
		// TmdbID and ImdbID identify the ITEM — for a series that is the
		// show, never the episode: an episode's own id is not what
		// identifies the series it belongs to. 0 / "" when unknown.
		TmdbID int64  `json:"tmdbId,omitempty"`
		ImdbID string `json:"imdbId,omitempty"`
		// DownloadID is the row this import came from, so a consumer can
		// write back onto its handoff trace.
		DownloadID int64 `json:"downloadId,omitempty"`
		// Transfer is the id that names this transfer end to end
		// (contract §3.1), so one grep spans every application it crossed.
		Transfer string `json:"transfer,omitempty"`
	}
	// ImportFailed is published when a completed download cannot import.
	ImportFailed struct {
		MediaItemID int64  `json:"mediaItemId"`
		Release     string `json:"release"`
		Reason      string `json:"reason"`
	}
)

// EventType implements bus.Event.
func (ReleaseGrabbed) EventType() string { return "release.grabbed" }

// EventType implements bus.Event.
func (ImportCompleted) EventType() string { return "import.completed" }

// EventType implements bus.Event.
func (ImportFailed) EventType() string { return "import.failed" }

// Service wires storage, adapters (via factories), and the bus.
type Service struct {
	db         *sqlite.DB
	bus        *bus.Bus
	log        *slog.Logger
	newIndexer IndexerFactory
	newClient  ClientFactory

	// searchTimeout bounds each indexer call.
	searchTimeout time.Duration

	// wanted caches the missing/upgradable index (Phase 3).
	wanted wantedIndex

	// One mutex per download id, so the 30 s poll and an event arriving
	// for the same download cannot both decide to import it. The map only
	// ever grows by the number of downloads seen in this process, which
	// is bounded by the queue, so it is not swept.
	reconcileMu sync.Mutex
	reconciling map[int64]*sync.Mutex

	// Live state of each push subscription, for the UI.
	linksMu sync.Mutex
	links   map[int64]*linkState

	// When each client last actually gave us something, and what it said if
	// it did not. A client can pass its Test while nothing has come through
	// it for an hour — that is what a subscription dying quietly looks like,
	// and reachability alone would call it healthy forever.
	contactMu sync.Mutex
	contacts  map[int64]health.Contact
}

// New returns a Service.
func New(db *sqlite.DB, b *bus.Bus, log *slog.Logger, ni IndexerFactory, nc ClientFactory) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, bus: b, log: log, newIndexer: ni, newClient: nc, searchTimeout: 30 * time.Second}
}

// noteContact records the outcome of one exchange with a client.
func (s *Service) noteContact(clientID int64, err error) {
	s.contactMu.Lock()
	defer s.contactMu.Unlock()
	if s.contacts == nil {
		s.contacts = map[int64]health.Contact{}
	}
	c := s.contacts[clientID]
	if err != nil {
		// The timestamp is "last time this WORKED", so a failure updates
		// the reason and deliberately leaves the clock where it was —
		// otherwise a client failing every 30 seconds would look freshly
		// contacted forever.
		c.Error = err.Error()
	} else {
		c.At, c.Error = time.Now(), ""
	}
	s.contacts[clientID] = c
}

// Contacts reports last-successful-contact per client, for the health
// checks and the connections panel.
func (s *Service) Contacts() map[int64]health.Contact {
	s.contactMu.Lock()
	defer s.contactMu.Unlock()
	out := make(map[int64]health.Contact, len(s.contacts))
	for k, v := range s.contacts {
		out[k] = v
	}
	return out
}

func (s *Service) publish(e bus.Event) {
	if s.bus != nil {
		s.bus.Publish(e)
	}
}

// ---- wantable construction ----

// episodeState is what ONE copy has on disk for one episode: whether a file is
// there at all, the best KNOWN quality among the files covering it, and whether
// that quality's source is trustworthy.
//
// The three are separate on purpose. "No file" and "a file whose quality we
// could not determine" used to be the same nil, and every decision path read
// it as the first — which is how a 17 GB file on disk got hunted as missing
// (ADR 0013).
type episodeState struct {
	HasFile  bool
	Have     *quality.Quality
	Verified bool
}

// episodeStates maps episode id → what one copy has for it (copyID 0 =
// primary). Copies never see each other's files.
func (s *Service) episodeStates(ctx context.Context, item domain.MediaItem, copyID int64) (map[int64]episodeState, error) {
	files, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	records, err := s.db.FileQualityRecords(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]sqlite.FileQuality, len(records))
	for _, r := range records {
		byID[r.FileID] = r
	}
	out := map[int64]episodeState{}
	for _, f := range files {
		if f.CopyID != copyID {
			continue
		}
		rec := byID[f.ID]
		// A measured-and-disbelieved file does not hold an episode. Same
		// reasoning as DiskStateForItem: HasFile exists to stop monarr
		// replacing a file it merely could not read, and this is not that.
		if rec.Provenance == mediainfo.ProvenanceImplausible {
			continue
		}
		for _, epID := range f.EpisodeIDs {
			st := out[epID]
			st.HasFile = true
			if rec.Known && (st.Have == nil || quality.Better(rec.Quality, *st.Have)) {
				q := rec.Quality
				st.Have = &q
				st.Verified = rec.SourceVerified()
			}
			out[epID] = st
		}
	}
	return out, nil
}

// Target resolves the wantable being searched/grabbed: movie, one episode,
// or a season pack — for the primary copy.
func (s *Service) target(ctx context.Context, item domain.MediaItem, season, episode int) (domain.Wantable, error) {
	return s.targetCopy(ctx, item, season, episode, nil)
}

// targetCopy is target for one specific media copy (nil = primary): the
// copy's profile and the copy's own file set drive the decision.
func (s *Service) targetCopy(ctx context.Context, item domain.MediaItem, season, episode int, cp *domain.MediaCopy) (domain.Wantable, error) {
	var copyID, profileID int64 = 0, item.QualityProfileID
	copyName := ""
	if cp != nil {
		copyID, profileID = cp.ID, cp.QualityProfileID
		copyName = copyLabel(*cp)
	}
	if item.Kind == domain.KindMovie || item.Kind == domain.KindBook {
		state, err := s.db.DiskStateForItem(ctx, item.ID, copyID)
		if err != nil {
			return nil, err
		}
		if item.Kind == domain.KindBook {
			return domain.BookWantable{
				Item: item.ID, Profile: profileID, Mon: item.Monitored,
				Title: item.Title, Author: item.Author, Year: item.Year,
				Have: state.Best, Files: state.HasFiles, Verified: state.SourceVerified,
			}, nil
		}
		mon := item.Monitored
		if cp != nil {
			mon = mon && cp.Monitored
		}
		return domain.MovieWantable{
			Item: item.ID, Profile: profileID, Mon: mon,
			Title: item.Title, Year: item.Year,
			Have: state.Best, Files: state.HasFiles, Verified: state.SourceVerified,
			Copy: copyID, CopyName: copyName,
		}, nil
	}

	epStates, err := s.episodeStates(ctx, item, copyID)
	if err != nil {
		return nil, err
	}
	var seasonObj *domain.Season
	for i := range item.Seasons {
		if item.Seasons[i].Number == season {
			seasonObj = &item.Seasons[i]
		}
	}
	if seasonObj == nil {
		return nil, fmt.Errorf("season %d: %w", season, ErrNotFound)
	}
	copyMon := cp == nil || cp.Monitored
	mkEp := func(e domain.Episode) domain.EpisodeWantable {
		st := epStates[e.ID]
		return domain.EpisodeWantable{
			Item: item.ID, EpisodeID: e.ID, Profile: profileID,
			Mon: item.Monitored && e.Monitored && copyMon, Title: item.Title, Year: item.Year,
			Season: e.SeasonNumber, Episode: e.EpisodeNumber,
			Have: st.Have, Files: st.HasFile, Verified: st.Verified,
			Absolute: e.AbsoluteNum, Copy: copyID, CopyName: copyName,
		}
	}
	if episode > 0 {
		for _, e := range seasonObj.Episodes {
			if e.EpisodeNumber == episode {
				return mkEp(e), nil
			}
		}
		return nil, fmt.Errorf("episode %d: %w", episode, ErrNotFound)
	}
	pack := domain.SeasonWantable{
		Item: item.ID, Profile: profileID,
		Mon: item.Monitored && seasonObj.Monitored && copyMon, Title: item.Title, Year: item.Year,
		Season: season, Copy: copyID, CopyName: copyName,
	}
	for _, e := range seasonObj.Episodes {
		pack.Episodes = append(pack.Episodes, mkEp(e))
	}
	return pack, nil
}

// copyLabel is the copy's display name: explicit name, else "copy N".
func copyLabel(c domain.MediaCopy) string {
	if c.Name != "" {
		return c.Name
	}
	return fmt.Sprintf("copy %d", c.ID)
}

// ---- interactive search ----

// Candidate is one release with its verdict, for the interactive search UI.
type Candidate struct {
	Release    ports.Release        `json:"release"`
	Quality    quality.Quality      `json:"-"`
	QualityStr string               `json:"quality"`
	Age        string               `json:"age"`
	Score      int                  `json:"score"`   // custom-format score sum
	Formats    []string             `json:"formats"` // matched custom formats
	Accepted   bool                 `json:"accepted"`
	IsUpgrade  bool                 `json:"isUpgrade"`
	Rejections []decision.Rejection `json:"rejections"`
	// Warning is a caution that does not decline the release: the profile
	// would take it, but something about it does not add up. Today that is
	// only an advertised size too small to hold what the name claims.
	Warning string `json:"warning,omitempty"`
}

// Search fans out to every enabled indexer, parses and judges every
// release against the target wantable, and returns ranked candidates with
// rejection reasons attached (never filtered out — upstream's best UX).
func (s *Service) Search(ctx context.Context, itemID int64, season, episode int) ([]Candidate, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return nil, err
	}
	target, err := s.target(ctx, item, season, episode)
	if err != nil {
		return nil, err
	}
	profile, err := s.db.GetProfile(ctx, item.QualityProfileID)
	if err != nil {
		return nil, err
	}
	indexers, err := s.db.ListIndexers(ctx)
	if err != nil {
		return nil, err
	}
	var enabled []ports.IndexerConfig
	for _, ic := range indexers {
		if ic.Enabled {
			enabled = append(enabled, ic)
		}
	}
	if len(enabled) == 0 {
		return nil, ErrNoIndexers
	}

	queries := domain.PlanSearch(target)
	var (
		mu       sync.Mutex
		releases []ports.Release
		wg       sync.WaitGroup
	)
	for _, cfg := range enabled {
		for _, q := range queries {
			wg.Add(1)
			go func(cfg ports.IndexerConfig, q domain.SearchQuery) {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
				defer cancel()
				rs, err := s.newIndexer(cfg).Search(cctx, q)
				if err != nil {
					s.log.Warn("search: indexer failed", "indexer", cfg.Name, "err", err)
					return
				}
				mu.Lock()
				releases = append(releases, rs...)
				mu.Unlock()
			}(cfg, q)
		}
	}
	wg.Wait()

	formats, _ := s.db.ListCustomFormats(ctx)
	runtimeMin := item.Runtime
	now := time.Now()
	seen := map[string]bool{}
	out := make([]Candidate, 0, len(releases))
	for _, r := range releases {
		key := r.Title + "|" + r.Indexer
		if seen[key] {
			continue
		}
		seen[key] = true

		p := parser.Parse(r.Title)
		c := Candidate{Release: r, Quality: p.Quality, QualityStr: p.Quality.Display(),
			Score: format.Score(r.Title, formats), Formats: format.Matches(r.Title, formats)}
		if !r.PublishDate.IsZero() {
			c.Age = age(now.Sub(r.PublishDate))
		}
		matches := matcher.Match(p, []domain.Wantable{target})
		if len(matches) == 0 {
			c.Rejections = []decision.Rejection{{
				Code:   "not_matched",
				Reason: fmt.Sprintf("does not match %s", describeTarget(target)),
			}}
		} else {
			d := decision.Decide(p.Quality, target, profile)
			c.Accepted = d.Accepted
			c.IsUpgrade = d.IsUpgrade
			c.Rejections = d.Rejections
		}
		// A size that cannot hold the claim is worth saying out loud even on a
		// release the profile would take. Attached as a warning, not a
		// rejection: the person reading this list can weigh it themselves, and
		// gating a manual grab is something monarr has never done.
		if why, bad := sizeImplausible(p.Quality, r, runtimeMin); bad {
			c.Warning = why.Reason
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Accepted != out[j].Accepted {
			return out[i].Accepted
		}
		ri, rj := quality.Rank(out[i].Quality), quality.Rank(out[j].Quality)
		if ri != rj {
			return ri > rj
		}
		// Same quality: custom-format score breaks the tie (Phase 5).
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Release.Seeders > out[j].Release.Seeders
	})
	return out, nil
}

func describeTarget(w domain.Wantable) string {
	switch t := w.(type) {
	case domain.MovieWantable:
		return fmt.Sprintf("%s (%d)", t.Title, t.Year)
	case domain.EpisodeWantable:
		return fmt.Sprintf("%s S%02dE%02d", t.Title, t.Season, t.Episode)
	case domain.SeasonWantable:
		return fmt.Sprintf("%s season %d", t.Title, t.Season)
	case domain.BookWantable:
		if t.Author != "" {
			return fmt.Sprintf("%s by %s", t.Title, t.Author)
		}
		return t.Title
	}
	return "target"
}

func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ---- grab ----

// GrabRequest is what the UI sends back from a chosen candidate.
type GrabRequest struct {
	MediaItemID int64
	CopyID      int64 // 0 = the primary copy
	Season      int   // -1 for movies
	Episode     int   // 0 = whole season / movie
	Title       string
	DownloadURL string
	Indexer     string
	Protocol    string
	Size        int64
}

func protocolOfClient(clientType string) string { return ports.ProtocolOfClient(clientType) }

// newTransferID mints the id that names one transfer end to end
// (nzbd/docs/INTEGRATION_PLAN.md §3.1): `t-<downloads.id>-<6 lowercase
// hex>`.
//
// The row id alone would be enough to be unique here, but not to be
// unambiguous THERE: ids restart when a database is rebuilt from a backup,
// and a stale "t-42" in a media server's log would then point at a
// different download entirely. The random half makes a collision across
// rebuilds something you would have to be unlucky twice to see.
func newTransferID(downloadID int64) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A transfer id that is merely unique-per-database is worth far
		// more than no id at all: without one the trace has no thread.
		return fmt.Sprintf("t-%d-%06x", downloadID, downloadID&0xffffff)
	}
	return fmt.Sprintf("t-%d-%x", downloadID, b)
}

// addToClient sends the release, carrying the transfer id when the client
// can hold one. The type assertion is the whole mechanism: clients that
// cannot tag a download are added exactly as before, and Monarr keeps the
// id on its own row so its trace is still threaded.
func addToClient(ctx context.Context, c ports.DownloadClient, downloadURL, category, transfer string) (ports.Handle, error) {
	if tagger, ok := c.(ports.TaggedAdder); ok && transfer != "" {
		return tagger.AddTagged(ctx, downloadURL, category, transfer)
	}
	return c.Add(ctx, downloadURL, category)
}

// Grab sends the release to the right client by protocol and records the
// queue row + history (blueprint §5.1 grab → import).
func (s *Service) Grab(ctx context.Context, req GrabRequest) (int64, error) {
	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return 0, err
	}
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return 0, err
	}
	var cfg *ports.ClientConfig
	for i := range clients {
		if clients[i].Enabled && protocolOfClient(clients[i].Type) == req.Protocol {
			cfg = &clients[i]
			break
		}
	}
	if cfg == nil {
		return 0, fmt.Errorf("%w: %s", ErrNoClient, req.Protocol)
	}

	p := parser.Parse(req.Title)
	var base string
	switch {
	case item.Kind == domain.KindMovie:
		base = fmt.Sprintf("movie:%d", item.ID)
	case item.Kind == domain.KindBook:
		base = fmt.Sprintf("book:%d", item.ID)
	case req.Episode > 0:
		base = fmt.Sprintf("episode:%d:%d:%d", item.ID, req.Season, req.Episode)
	default:
		base = fmt.Sprintf("season:%d:%d", item.ID, req.Season)
	}
	if req.CopyID != 0 {
		base = fmt.Sprintf("%s:c%d", base, req.CopyID)
	}
	wants := []string{base}

	// The row goes in BEFORE the client is asked to take the download,
	// because the transfer id is built from the row id and has to be on
	// the request that creates the download — a client cannot be told
	// afterwards what to have called it. A client that then refuses the
	// add leaves a row that describes a download nobody has, so it is
	// removed again below; the alternative (add first, insert second) is
	// a download running in a client that Monarr has no record of, which
	// is the worse of the two failures by a distance.
	id, err := s.db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: item.ID, CopyID: req.CopyID, WantableIDs: wants, Season: req.Season,
		ReleaseTitle: req.Title, Indexer: req.Indexer, Protocol: req.Protocol,
		Quality: p.Quality, Size: req.Size, ClientID: cfg.ID,
		State: "grabbed",
	})
	if err != nil {
		return 0, err
	}
	transfer := newTransferID(id)
	handle, err := addToClient(ctx, s.newClient(*cfg), req.DownloadURL, cfg.Category, transfer)
	if err != nil {
		// Nothing downstream ever saw this row: no event, no trace entry,
		// no history. Dropping it is the honest undo.
		if delErr := s.db.DeleteDownload(ctx, id); delErr != nil {
			s.log.Warn("grab: could not remove the row for a rejected add",
				"download", id, "err", delErr)
		}
		return 0, err
	}
	if err := s.db.SetDownloadHandle(ctx, id, string(handle), transfer); err != nil {
		// The download IS running; losing the handle would orphan it, so
		// this is loud rather than fatal — the title-match fallback in
		// matchStatus still reconciles it.
		s.log.Error("grab: download accepted but its handle could not be stored",
			"download", id, "handle", handle, "err", err)
	}
	// Open the handoff trace so every step from here is laid out.
	dl := sqlite.Download{ID: id, MediaItemID: item.ID, ReleaseTitle: req.Title,
		State: "grabbed", Transfer: transfer}
	detail := "sent to " + clientLabel(*cfg)
	if transfer != "" {
		// The id is in the trace's first line because that is where
		// someone starts reading when a transfer goes wrong.
		detail += " as " + transfer
	}
	s.advance(ctx, &dl, "grabbed", 0, "", stepGrabbed, detail)
	_ = s.db.AddHistory(ctx, "grabbed", item.ID, req.Title,
		map[string]any{"indexer": req.Indexer, "protocol": req.Protocol})
	s.publish(ReleaseGrabbed{MediaItemID: item.ID, Title: req.Title,
		Indexer: req.Indexer, Protocol: req.Protocol})
	s.log.Info("grabbed", "title", req.Title, "client", cfg.Name)
	return id, nil
}

// ---- queue ----

// RefreshQueue polls clients and reconciles queue rows through the state
// machine; completed downloads are imported (blueprint §5.1).
//
// Every enabled client is polled on every tick, including when Monarr has
// nothing in flight with it. That is deliberate, and it was not always so:
// the poll used to start from the active-downloads list and return early when
// it was empty, which meant an idle Monarr never spoke to its client at all.
// Three things broke as a result, and all three looked like the client's
// fault rather than ours:
//
//   - the contact clock froze, so `client:<name>` went WARNING at five
//     minutes and ERROR at thirty on any instance that simply had nothing
//     downloading. A check that cannot be green while you are idle is not a
//     health check, it is a clock.
//   - the Connections panel degraded off the same frozen clock, with no
//     detail to explain it.
//   - the client never saw a history read, so a downloader that records who
//     collected a finished job — nzbd does — reported every completed
//     download as never picked up.
//
// Polling an idle client costs two small GETs every 30 s. Being wrong about
// whether the pipeline is alive costs considerably more.
func (s *Service) RefreshQueue(ctx context.Context) error {
	active, err := s.db.ListActiveDownloads(ctx)
	if err != nil {
		return err
	}
	byClient := map[int64][]sqlite.Download{}
	for _, dl := range active {
		byClient[dl.ClientID] = append(byClient[dl.ClientID], dl)
	}

	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return err
	}
	for _, cfg := range clients {
		if !cfg.Enabled {
			continue
		}
		statuses, err := s.newClient(cfg).Statuses(ctx)
		s.noteContact(cfg.ID, err)
		if err != nil {
			s.log.Warn("queue: client poll failed", "client", cfg.Name, "err", err)
			continue
		}
		for _, dl := range byClient[cfg.ID] {
			st, ok := matchStatus(dl, statuses)
			if !ok {
				continue // not visible yet (magnet resolving, etc.)
			}
			s.reconcileDownload(ctx, dl, cfg, st, "poll")
		}
	}
	return nil
}

// reconcileDownload advances ONE download from ONE observation of it.
//
// Poll and push both land here, deliberately: the 30 s sweep and the event
// stream are two ways of learning the same fact, and the moment they have
// separate state machines they will disagree about what a download is
// doing. `source` only colors the trace — the decisions are identical.
//
// Serialized per download id, because the two channels genuinely race: an
// event can arrive in the same instant a poll tick reads the same client.
// Without the lock both would see state 'downloaded' and both would run
// the import, which places the files twice and writes two 'imported'
// entries into a trace whose whole job is to be readable.
func (s *Service) reconcileDownload(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, st ports.DownloadStatus, source string) {
	unlock := s.lockDownload(dl.ID)
	defer unlock()

	// Re-read under the lock: whoever held it before us may have moved
	// this row on, and acting on the copy we were handed would undo them.
	if fresh, err := s.db.GetDownload(ctx, dl.ID); err == nil {
		dl = fresh
	}

	switch st.State {
	case ports.StateQueued, ports.StateDownloading:
		if dl.State == "grabbed" {
			// First sighting in the client queue — log the step.
			detail := "download client is fetching the release"
			if st.Message != "" {
				detail = st.Message
			}
			s.advance(ctx, &dl, "downloading", st.Progress, "", stepDownloading,
				traced(detail, source))
		} else {
			_ = s.db.UpdateDownloadState(ctx, dl.ID, "downloading", st.Progress, "")
		}
	case ports.StateFailed:
		if dl.State == "imported" || dl.State == "failed" {
			return
		}
		// A client-reported failure is normally a bad release: blocklist it
		// and search a replacement. Unless the client says the release is
		// not to blame — an operator deleting the job is not a reason to
		// ban the release forever.
		s.handleFailure(ctx, dl, st.Progress, st.Message, st.Blameless)
	case ports.StateCompleted:
		// 'downloaded' and 'importing' are NOT terminal, but they mean
		// someone is already on it; only 'grabbed'/'downloading' should
		// start an import from here.
		switch dl.State {
		case "imported", "awaiting_import", "importing", "downloaded", "failed":
			return
		}
		s.onDownloaded(ctx, dl, cfg, st, source)
	}
}

// traced names the channel that delivered an observation. "completed
// (event 12)" versus "completed (poll)" is the difference between knowing
// push is working and assuming it is.
func traced(detail, source string) string {
	if source == "" {
		return detail
	}
	return detail + " (" + source + ")"
}

// lockDownload serializes work on one download id across the poller and
// the event subscribers, returning the unlock.
func (s *Service) lockDownload(id int64) func() {
	s.reconcileMu.Lock()
	if s.reconciling == nil {
		s.reconciling = map[int64]*sync.Mutex{}
	}
	m, ok := s.reconciling[id]
	if !ok {
		m = &sync.Mutex{}
		s.reconciling[id] = m
	}
	s.reconcileMu.Unlock()
	m.Lock()
	return m.Unlock
}

// handleFailure marks a download failed, blocklists the release so it is
// never re-grabbed, and immediately re-searches for the affected wantables
// (blueprint §5.1 "failed-download handling").
func (s *Service) handleFailure(ctx context.Context, dl sqlite.Download, progress float64, reason string, blameless bool) {
	s.advance(ctx, &dl, "failed", progress, reason, stepFailed, "download failed: "+reason)
	_ = s.db.AddHistory(ctx, "failed", dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"reason": reason})
	if blameless {
		// The download stopped, but not because the release is bad. Banning
		// it would burn a good copy and send the re-search after a worse one.
		s.log.Warn("download failed; NOT blocklisted (the release is not at fault)",
			"release", dl.ReleaseTitle, "reason", reason)
	} else if err := s.db.AddBlocklist(ctx, dl.MediaItemID, dl.ReleaseTitle, dl.Indexer, reason); err != nil {
		s.log.Warn("blocklist: insert failed", "release", dl.ReleaseTitle, "err", err)
	} else {
		s.log.Warn("download failed; blocklisted", "release", dl.ReleaseTitle, "reason", reason)
	}
	s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: reason})

	// Automatic re-search: try to replace the failed grab right away.
	enabled, err := s.enabledIndexers(ctx)
	if err != nil || len(enabled) == 0 {
		return
	}
	for _, idStr := range dl.WantableIDs {
		w, err := s.wantableFromID(ctx, idStr)
		if err != nil {
			continue
		}
		if err := s.searchAndGrabBest(ctx, w, enabled); err != nil {
			s.log.Warn("re-search failed", "wantable", idStr, "err", err)
		}
	}
}

// wantableFromID reconstructs a wantable from its stable id string
// ("movie:5", "episode:5:2:3", "season:5:2", "book:9").
func (s *Service) wantableFromID(ctx context.Context, id string) (domain.Wantable, error) {
	// A ":c<n>" suffix pins the wantable to a media copy.
	var copyID int64
	if i := strings.LastIndex(id, ":c"); i > 0 {
		if n, _ := fmt.Sscanf(id[i:], ":c%d", &copyID); n == 1 {
			id = id[:i]
		}
	}
	var kind string
	var a, b, c int64
	n, _ := fmt.Sscanf(id, "movie:%d", &a)
	if n == 1 {
		kind = "flat"
	} else if n, _ = fmt.Sscanf(id, "book:%d", &a); n == 1 {
		kind = "flat"
	} else if n, _ = fmt.Sscanf(id, "episode:%d:%d:%d", &a, &b, &c); n == 3 {
		kind = "episode"
	} else if n, _ = fmt.Sscanf(id, "season:%d:%d", &a, &b); n == 2 {
		kind = "season"
	} else {
		return nil, fmt.Errorf("unparseable wantable id %q", id)
	}
	item, err := s.db.GetMediaItemFull(ctx, a)
	if err != nil {
		return nil, err
	}
	var cp *domain.MediaCopy
	if copyID != 0 {
		mc, err := s.db.GetMediaCopy(ctx, a, copyID)
		if err != nil {
			return nil, err
		}
		cp = &mc
	}
	switch kind {
	case "episode":
		return s.targetCopy(ctx, item, int(b), int(c), cp)
	case "season":
		return s.targetCopy(ctx, item, int(b), 0, cp)
	default:
		return s.targetCopy(ctx, item, 0, 0, cp)
	}
}

func matchStatus(dl sqlite.Download, statuses []ports.DownloadStatus) (ports.DownloadStatus, bool) {
	if dl.Handle != "" {
		for _, st := range statuses {
			if string(st.Handle) == dl.Handle {
				return st, true
			}
		}
	}
	norm := func(s string) string {
		return strings.ToLower(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(s))
	}
	want := norm(dl.ReleaseTitle)
	for _, st := range statuses {
		if norm(st.Name) == want {
			return st, true
		}
	}
	return ports.DownloadStatus{}, false
}

// Queue returns recent downloads for the UI.
func (s *Service) Queue(ctx context.Context) ([]sqlite.Download, error) {
	return s.db.ListRecentDownloads(ctx)
}

// RemoveDownload deletes a queue row and optionally the client-side item.
func (s *Service) RemoveDownload(ctx context.Context, id int64, fromClient bool) error {
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	if fromClient && dl.Handle != "" {
		if cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID); err == nil {
			_ = s.newClient(cfg).Remove(ctx, ports.Handle(dl.Handle), false)
		}
	}
	return s.db.DeleteDownload(ctx, id)
}
