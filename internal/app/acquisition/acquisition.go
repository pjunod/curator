// Package acquisition is the Phase 2 orchestrator: interactive search
// (fan out → parse → match → decide, rejection reasons attached), grabbing
// to the right download client, queue tracking, and import (blueprint §5.1).
package acquisition

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/decision"
	"github.com/monarr-media/monarr/internal/domain/matcher"
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
	ImportCompleted struct {
		MediaItemID int64  `json:"mediaItemId"`
		Release     string `json:"release"`
		Files       int    `json:"files"`
		Upgrade     bool   `json:"upgrade"`
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
}

// New returns a Service.
func New(db *sqlite.DB, b *bus.Bus, log *slog.Logger, ni IndexerFactory, nc ClientFactory) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, bus: b, log: log, newIndexer: ni, newClient: nc, searchTimeout: 30 * time.Second}
}

func (s *Service) publish(e bus.Event) {
	if s.bus != nil {
		s.bus.Publish(e)
	}
}

// ---- wantable construction ----

// episodeQualities maps episode id → best on-disk quality.
func (s *Service) episodeQualities(ctx context.Context, item domain.MediaItem) (map[int64]*quality.Quality, error) {
	files, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	qualities, err := s.db.FileQualities(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	out := map[int64]*quality.Quality{}
	for _, f := range files {
		q, ok := qualities[f.ID]
		if !ok {
			continue
		}
		for _, epID := range f.EpisodeIDs {
			if cur := out[epID]; cur == nil || quality.Better(q, *cur) {
				qq := q
				out[epID] = &qq
			}
		}
	}
	return out, nil
}

// Target resolves the wantable being searched/grabbed: movie, one episode,
// or a season pack.
func (s *Service) target(ctx context.Context, item domain.MediaItem, season, episode int) (domain.Wantable, error) {
	if item.Kind == domain.KindMovie {
		w := domain.MovieWantable{
			Item: item.ID, Profile: item.QualityProfileID, Mon: item.Monitored,
			Title: item.Title, Year: item.Year,
		}
		if q, ok, err := s.db.BestQualityForItem(ctx, item.ID); err != nil {
			return nil, err
		} else if ok {
			w.Have = &q
		}
		return w, nil
	}

	epQuals, err := s.episodeQualities(ctx, item)
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
	mkEp := func(e domain.Episode) domain.EpisodeWantable {
		return domain.EpisodeWantable{
			Item: item.ID, EpisodeID: e.ID, Profile: item.QualityProfileID,
			Mon: item.Monitored && e.Monitored, Title: item.Title, Year: item.Year,
			Season: e.SeasonNumber, Episode: e.EpisodeNumber, Have: epQuals[e.ID],
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
		Item: item.ID, Profile: item.QualityProfileID,
		Mon: item.Monitored && seasonObj.Monitored, Title: item.Title, Year: item.Year,
		Season: season,
	}
	for _, e := range seasonObj.Episodes {
		pack.Episodes = append(pack.Episodes, mkEp(e))
	}
	return pack, nil
}

// ---- interactive search ----

// Candidate is one release with its verdict, for the interactive search UI.
type Candidate struct {
	Release    ports.Release        `json:"release"`
	Quality    quality.Quality      `json:"-"`
	QualityStr string               `json:"quality"`
	Age        string               `json:"age"`
	Accepted   bool                 `json:"accepted"`
	IsUpgrade  bool                 `json:"isUpgrade"`
	Rejections []decision.Rejection `json:"rejections"`
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
		c := Candidate{Release: r, Quality: p.Quality, QualityStr: p.Quality.Display()}
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
	Season      int // -1 for movies
	Episode     int // 0 = whole season / movie
	Title       string
	DownloadURL string
	Indexer     string
	Protocol    string
	Size        int64
}

func protocolOfClient(clientType string) string {
	if clientType == "sabnzbd" {
		return "usenet"
	}
	return "torrent"
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

	handle, err := s.newClient(*cfg).Add(ctx, req.DownloadURL, cfg.Category)
	if err != nil {
		return 0, err
	}

	p := parser.Parse(req.Title)
	var wants []string
	switch {
	case item.Kind == domain.KindMovie:
		wants = []string{fmt.Sprintf("movie:%d", item.ID)}
	case req.Episode > 0:
		wants = []string{fmt.Sprintf("episode:%d:%d:%d", item.ID, req.Season, req.Episode)}
	default:
		wants = []string{fmt.Sprintf("season:%d:%d", item.ID, req.Season)}
	}

	id, err := s.db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: item.ID, WantableIDs: wants, Season: req.Season,
		ReleaseTitle: req.Title, Indexer: req.Indexer, Protocol: req.Protocol,
		Quality: p.Quality, Size: req.Size, ClientID: cfg.ID,
		Handle: string(handle), State: "grabbed",
	})
	if err != nil {
		return 0, err
	}
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
func (s *Service) RefreshQueue(ctx context.Context) error {
	active, err := s.db.ListActiveDownloads(ctx)
	if err != nil || len(active) == 0 {
		return err
	}

	byClient := map[int64][]sqlite.Download{}
	for _, dl := range active {
		byClient[dl.ClientID] = append(byClient[dl.ClientID], dl)
	}
	for clientID, dls := range byClient {
		cfg, err := s.db.GetDownloadClient(ctx, clientID)
		if err != nil {
			continue
		}
		statuses, err := s.newClient(cfg).Statuses(ctx)
		if err != nil {
			s.log.Warn("queue: client poll failed", "client", cfg.Name, "err", err)
			continue
		}
		for _, dl := range dls {
			st, ok := matchStatus(dl, statuses)
			if !ok {
				continue // not visible yet (magnet resolving, etc.)
			}
			switch st.State {
			case ports.StateQueued, ports.StateDownloading:
				_ = s.db.UpdateDownloadState(ctx, dl.ID, "downloading", st.Progress, "")
			case ports.StateFailed:
				_ = s.db.UpdateDownloadState(ctx, dl.ID, "failed", st.Progress, st.Message)
				_ = s.db.AddHistory(ctx, "failed", dl.MediaItemID, dl.ReleaseTitle,
					map[string]any{"reason": st.Message})
				s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: st.Message})
			case ports.StateCompleted:
				if dl.State == "imported" {
					continue
				}
				_ = s.db.UpdateDownloadState(ctx, dl.ID, "importing", 1, "")
				if err := s.importDownload(ctx, dl, st.SavePath); err != nil {
					_ = s.db.UpdateDownloadState(ctx, dl.ID, "failed", 1, err.Error())
					_ = s.db.AddHistory(ctx, "failed", dl.MediaItemID, dl.ReleaseTitle,
						map[string]any{"reason": err.Error()})
					s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: err.Error()})
				} else {
					_ = s.db.UpdateDownloadState(ctx, dl.ID, "imported", 1, "")
				}
			}
		}
	}
	return nil
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
