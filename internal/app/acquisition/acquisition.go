// Package acquisition is the Phase 2 orchestrator: interactive search
// (fan out → parse → match → decide, rejection reasons attached), grabbing
// to the right download client, queue tracking, and import (blueprint §5.1).
package acquisition

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/format"
	"github.com/pjunod/monarr/internal/domain/matcher"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Factories let cmd/monarr wire real adapters while tests inject fakes —
// the app layer depends on ports only.
type (
	IndexerFactory func(ports.IndexerConfig) ports.Indexer
	ClientFactory  func(ports.ClientConfig) ports.DownloadClient
)

// Errors surfaced to the API layer.
var (
	ErrNoIndexers                    = errors.New("no enabled indexers configured")
	ErrNoClient                      = errors.New("no enabled download client for this protocol")
	ErrNoLibraryFolder               = errors.New("item has no library folder assigned")
	ErrNotFound                      = sqlite.ErrNotFound
	ErrRequestConflict               = errors.New("request ID was already used for a different manual grab")
	ErrBlacklistConfirmationRequired = errors.New("release is blacklisted; explicit one-time manual override required")
	ErrReplacementReviewRequired     = errors.New("use the reviewed replacement workflow for bad or quality decisions")
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
		// is three different things downstream, and the plurx notifier needs
		// the distinction to shape ids versus explicit Books work/edition scans.
		MediaItemKind string `json:"mediaItemKind,omitempty"`
		Title         string `json:"title,omitempty"`
		// Book metadata is explicit Curator knowledge carried to Cinema at the
		// import boundary. WorkID is the only cross-edition relationship key;
		// title and author are display facts and are never used as identity.
		BookTitle     string `json:"bookTitle,omitempty"`
		BookAuthor    string `json:"bookAuthor,omitempty"`
		BookMedium    string `json:"bookMedium,omitempty"`
		BookWorkID    string `json:"bookWorkId,omitempty"`
		BookEditionID string `json:"bookEditionId,omitempty"`
		BookCoverURL  string `json:"bookCoverUrl,omitempty"`
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
	completedMu       sync.Mutex
	storageDecisionMu sync.Mutex
	// Shared by ordinary and recovery imports, including crash reconciliation.
	importTargetMu    sync.Mutex
	recoveryMu        sync.Mutex
	recoveryReceiptMu sync.Mutex
	recoveryCancels   map[string]context.CancelFunc
	db                *sqlite.DB
	bus               *bus.Bus
	log               *slog.Logger
	rssSweepMu        sync.Mutex
	planDispatchMu    sync.Mutex
	newIndexer        IndexerFactory
	newClient         ClientFactory

	// searchTimeout bounds each indexer call.
	searchTimeout time.Duration

	// wanted caches the missing/upgradable index (Phase 3).
	wanted           wantedIndex
	wantedQueue      wantedJobEnqueuer
	identityMu       sync.Mutex
	identityRevision int64
	identityCache    matcher.IdentityIndex
	candidateMu      sync.Mutex
	candidateTokens  map[string]candidateRecord
	reservations     acquisitionReservations

	// One mutex per download id, so the 30 s poll and an event arriving
	// for the same download cannot both decide to import it. The map only
	// ever grows by the number of downloads seen in this process, which
	// is bounded by the queue, so it is not swept.
	reconcileMu sync.Mutex
	reconciling map[int64]*sync.Mutex
	// Acceptance attempts may be reached from the queue scheduler and from
	// import workers that already own a download lock. Serialize by receipt in
	// a separate lock domain so either path rechecks the persisted deadline
	// immediately before dispatch without recursive download locking.
	deliveryAckMu       sync.Mutex
	deliveryAckLocks    map[int64]*sync.Mutex
	deliverySourceMu    sync.Mutex
	deliverySourceLocks map[int64]*sync.Mutex

	// Live state of each push subscription, for the UI.
	linksMu sync.Mutex
	links   map[int64]*linkState

	// When each client last actually gave us something, and what it said if
	// it did not. A client can pass its Test while nothing has come through
	// it for an hour — that is what a subscription dying quietly looks like,
	// and reachability alone would call it healthy forever.
	contactMu sync.Mutex
	contacts  map[int64]health.Contact

	deliveryCapabilityMu sync.RWMutex
	deliveryCapabilities map[int64]bool

	// The data plane: import workers and the in-flight registry. See
	// importers.go for why they are not on the poll goroutine.
	importerFields
}

type runnerAdmissionPollPermitKey struct{}

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
	Audio    quality.Audio
}

func mediaIdentity(item domain.MediaItem) domain.MediaIdentity {
	return domain.MediaIdentity{
		IDs: item.IDs, Title: item.Title, Year: item.Year,
		Aliases:   append([]domain.TitleAlias(nil), item.Aliases...),
		Countries: append([]domain.CountryEvidence(nil), item.Countries...),
		Sources:   append([]domain.IdentitySourceStatus(nil), item.IdentitySources...),
	}
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
				st.Audio = rec.AudioOf()
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
			profile, err := s.db.GetProfile(ctx, profileID)
			if err != nil {
				return nil, err
			}
			bookType := item.BookType
			monitored := item.Monitored
			if cp != nil {
				bookType = cp.BookType
				monitored = monitored && cp.Monitored
			}
			// In-memory fixtures and pre-ADR callers may omit the explicit field;
			// persisted rows are backfilled by migration 0029.
			if !quality.ValidBookType(bookType) {
				bookType = quality.BookTypeForSource(profile.Target.Source)
			}
			return domain.BookWantable{
				Item: item.ID, Profile: profileID, Mon: monitored,
				Title: item.Title, Author: item.Author, Year: item.Year,
				Have: state.Best, Files: state.HasFiles, Verified: state.SourceVerified,
				Audio: state.Audio, BookType: bookType, Copy: copyID, CopyName: copyName,
			}, nil
		}
		mon := item.Monitored
		if cp != nil {
			mon = mon && cp.Monitored
		}
		return domain.MovieWantable{
			Item: item.ID, Profile: profileID, Mon: mon,
			Title: item.Title, Year: item.Year, Identity: mediaIdentity(item),
			Have: state.Best, Files: state.HasFiles, Verified: state.SourceVerified,
			Audio: state.Audio, Copy: copyID, CopyName: copyName,
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
			Identity: mediaIdentity(item),
			Season:   e.SeasonNumber, Episode: e.EpisodeNumber,
			Have: st.Have, Files: st.HasFile, Verified: st.Verified, Audio: st.Audio,
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
		Identity: mediaIdentity(item),
		Season:   season, Copy: copyID, CopyName: copyName,
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
	if quality.ValidBookType(c.BookType) {
		return string(c.BookType)
	}
	return fmt.Sprintf("copy %d", c.ID)
}

// ---- interactive search ----

// Candidate is one release with its verdict, for the interactive search UI.
type Candidate struct {
	Release    ports.Release   `json:"release"`
	Quality    quality.Quality `json:"-"`
	QualityStr string          `json:"quality"`
	// Languages are the audio languages the name advertises (ADR 0022),
	// canonical codes; "mul" for a MULTi/DUAL release.
	Languages  []string             `json:"languages"`
	Age        string               `json:"age"`
	Score      int                  `json:"score"`   // custom-format score sum
	Formats    []string             `json:"formats"` // matched custom formats
	Accepted   bool                 `json:"accepted"`
	IsUpgrade  bool                 `json:"isUpgrade"`
	Rejections []decision.Rejection `json:"rejections"`
	Match      domain.MatchEvidence `json:"match"`
	Token      string               `json:"candidateToken"`
	// Warning is a caution that does not decline the release: the profile
	// would take it, but something about it does not add up. Today that is
	// only an advertised size too small to hold what the name claims.
	Warning         string `json:"warning,omitempty"`
	BlacklistReason string `json:"blacklistReason,omitempty"`
}

type candidateRecord struct {
	expiresAt                         time.Time
	itemID, copyID                    int64
	indexerID                         int64
	season, episode                   int
	title, downloadURL, indexer, guid string
	evidence                          domain.MatchEvidence
}

func (s *Service) issueCandidateToken(itemID, copyID int64, season, episode int, release ports.Release, evidence domain.MatchEvidence) string {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ""
	}
	token := fmt.Sprintf("%x", random)
	now := time.Now()
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	if s.candidateTokens == nil {
		s.candidateTokens = make(map[string]candidateRecord)
	}
	for key, record := range s.candidateTokens {
		if now.After(record.expiresAt) {
			delete(s.candidateTokens, key)
		}
	}
	// Bound memory without evicting unexpired provenance tokens. A failed
	// admission (for example, a newly blocklisted release) must be retryable
	// with the same token and identity.
	if len(s.candidateTokens) >= 2048 {
		return ""
	}
	s.candidateTokens[token] = candidateRecord{
		expiresAt: now.Add(10 * time.Minute), itemID: itemID, copyID: copyID,
		season: season, episode: episode, title: release.Title,
		downloadURL: release.DownloadURL, indexer: release.Indexer, guid: release.GUID,
		indexerID: release.IndexerID, evidence: evidence,
	}
	return token
}

func (s *Service) consumeCandidateToken(req GrabRequest) (domain.MatchEvidence, string, int64, bool) {
	if req.CandidateToken == "" {
		return domain.MatchEvidence{}, "", 0, false
	}
	s.candidateMu.Lock()
	defer s.candidateMu.Unlock()
	record, ok := s.candidateTokens[req.CandidateToken]
	// A movie or book search issues its token with season 0 (the query has no
	// season), while the grab handler defaults an omitted season to -1. Both
	// mean "no season"; comparing them raw rejected every whole-item grab and
	// downgraded its retained evidence to a manual override.
	if !ok || time.Now().After(record.expiresAt) || record.itemID != req.MediaItemID ||
		record.copyID != req.CopyID || noSeason(record.season) != noSeason(req.Season) || record.episode != req.Episode ||
		record.title != req.Title || record.downloadURL != req.DownloadURL || record.indexer != req.Indexer {
		return domain.MatchEvidence{}, "", 0, false
	}
	return record.evidence, record.guid, record.indexerID, true
}

// noSeason folds the two "no season" spellings (0 from a query without one,
// -1 from a grab without one) onto one value for token comparison.
func noSeason(season int) int {
	if season < 0 {
		return 0
	}
	return season
}

// Search fans out to every enabled indexer, parses and judges every
// release against the target wantable, and returns ranked candidates with
// rejection reasons attached (never filtered out — upstream's best UX).
func (s *Service) Search(ctx context.Context, itemID int64, season, episode int) ([]Candidate, error) {
	return s.SearchCopy(ctx, itemID, 0, season, episode)
}

// SearchCopy is interactive search for one independently curated target.
// copyID zero selects the primary; a non-zero book copy is the other edition,
// and a non-zero video copy is an additional quality target.
func (s *Service) SearchCopy(ctx context.Context, itemID, copyID int64, season, episode int) ([]Candidate, error) {
	candidates, _, err := s.SearchCopyDetailed(ctx, itemID, copyID, season, episode)
	return candidates, err
}

// SearchCopyDetailed also reports per-indexer failures so callers can
// distinguish an authoritative empty result from an incomplete search.
func (s *Service) SearchCopyDetailed(ctx context.Context, itemID, copyID int64, season, episode int) ([]Candidate, SearchStatus, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return nil, SearchStatus{}, err
	}
	var cp *domain.MediaCopy
	if copyID != 0 {
		copy, err := s.db.GetMediaCopy(ctx, itemID, copyID)
		if err != nil {
			return nil, SearchStatus{}, err
		}
		cp = &copy
	}
	target, err := s.targetCopy(ctx, item, season, episode, cp)
	if err != nil {
		return nil, SearchStatus{}, err
	}
	profile, err := s.db.GetProfile(ctx, target.ProfileID())
	if err != nil {
		return nil, SearchStatus{}, err
	}
	indexers, err := s.db.ListIndexers(ctx)
	if err != nil {
		return nil, SearchStatus{}, err
	}
	var enabled []ports.IndexerConfig
	for _, ic := range indexers {
		if ic.Enabled {
			enabled = append(enabled, ic)
		}
	}
	if len(enabled) == 0 {
		return nil, SearchStatus{}, ErrNoIndexers
	}

	var (
		mu       sync.Mutex
		releases []ports.Release
		status   SearchStatus
		wg       sync.WaitGroup
	)
	for _, cfg := range enabled {
		wg.Add(1)
		go func(cfg ports.IndexerConfig) {
			defer wg.Done()
			indexer := s.budgetIndexer(cfg, "interactive", false)
			rs, reasons := s.executeIndexerSearch(ctx, indexer, target, true, nil)
			mu.Lock()
			releases = append(releases, rs...)
			for _, reason := range reasons {
				status.Reasons = append(status.Reasons, cfg.Name+": "+reason)
			}
			status.Partial = status.Partial || len(reasons) > 0
			mu.Unlock()
		}(cfg)
	}
	wg.Wait()

	formats, _ := s.db.ListCustomFormats(ctx)
	runtimeMin := item.Runtime
	now := time.Now()
	identityIndex, err := s.allIdentity(ctx, now)
	if err != nil {
		return nil, status, err
	}
	releases = deduplicateReleases(releases)
	out := make([]Candidate, 0, len(releases))
	for _, r := range releases {
		p := parser.Parse(r.Title)
		c := Candidate{Release: r, Quality: p.Quality, QualityStr: p.Quality.Display(),
			Languages: p.Languages,
			Score:     format.Score(r.Title, formats), Formats: format.Matches(r.Title, formats)}
		c.BlacklistReason, _, err = s.db.BlocklistReason(ctx, r.Title, r.Indexer)
		if err != nil {
			return nil, status, err
		}
		if r.GUID != "" && r.IndexerID != 0 {
			if reason, blocked, e := s.db.ReleaseIdentityBlocklistReason(ctx, itemID, r.IndexerID, r.GUID); e != nil {
				return nil, status, e
			} else if blocked {
				c.BlacklistReason = reason
			}
		}
		if !r.PublishDate.IsZero() {
			c.Age = age(now.Sub(r.PublishDate))
		}
		match := releaseMatch(r, p, target, identityIndex)
		c.Match = matchEvidence(p, target, r, match)
		c.Token = s.issueCandidateToken(itemID, copyID, season, episode, r, c.Match)
		if !match.Matched {
			c.Rejections = []decision.Rejection{{
				Code:   "not_matched",
				Reason: match.Reason,
			}}
		} else {
			d := decision.Decide(releaseOf(p), target, profile)
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
	return out, status, nil
}

// releaseOf is what the decision engine judges a parsed name by: the quality
// it claims and the languages it advertises (ADR 0022).
func releaseOf(p parser.Parsed) decision.Release {
	return decision.Release{Quality: p.Quality, Languages: p.Languages}
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
		prefix := ""
		switch t.BookType {
		case quality.BookTypeEbook:
			prefix = "Ebook: "
		case quality.BookTypeAudiobook:
			prefix = "Audiobook: "
		}
		if t.Author != "" {
			return fmt.Sprintf("%s%s by %s", prefix, t.Title, t.Author)
		}
		return prefix + t.Title
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
	Intent                 string
	MediaItemID            int64
	CopyID                 int64 // 0 = the primary copy
	Season                 int   // -1 for movies
	Episode                int   // 0 = whole season / movie
	Title                  string
	DownloadURL            string
	Indexer                string
	SourceGUID             string
	IndexerID              int64
	Protocol               string
	Size                   int64
	CandidateToken         string
	MatchEvidence          *domain.MatchEvidence
	RequestID              string
	AllowBlacklisted       bool
	ReplacementOperationID string
}

const (
	GrabIntentAutomatic        = "automatic"
	GrabIntentManualAdditional = "manual_additional"
)

type manualSubmissionPayload struct {
	URL      string `json:"url"`
	Category string `json:"category"`
	Priority int    `json:"priority"`
}

func manualRequestFingerprint(req GrabRequest, sourceCopyID int64) (string, error) {
	identity := struct {
		MediaItemID int64  `json:"mediaItemId"`
		CopyID      int64  `json:"copyId"`
		Season      int    `json:"season"`
		Episode     int    `json:"episode"`
		Title       string `json:"title"`
		DownloadURL string `json:"downloadUrl"`
		Indexer     string `json:"indexer"`
		Protocol    string `json:"protocol"`
		Size        int64  `json:"size"`
		Override    bool   `json:"downloadAnyway"`
	}{req.MediaItemID, sourceCopyID, req.Season, req.Episode, req.Title, req.DownloadURL,
		req.Indexer, req.Protocol, req.Size, req.AllowBlacklisted}
	raw, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func manualCopyPath(targetPath, requestID string, claims []sqlite.FolderClaim) (string, string) {
	sum := sha256.Sum256([]byte(requestID))
	suffix := hex.EncodeToString(sum[:5])
	base := filepath.Join(filepath.Dir(targetPath), filepath.Base(targetPath)+" (Manual copy "+suffix+")")
	taken := make(map[string]bool, len(claims))
	for _, claim := range claims {
		taken[filepath.Clean(claim.Path)] = true
	}
	path := base
	for n := 2; taken[filepath.Clean(path)]; n++ {
		path = fmt.Sprintf("%s (%d)", base, n)
	}
	return "Manual copy " + suffix, path
}

func protocolOfClient(clientType string) string { return ports.ProtocolOfClient(clientType) }

// newTransferID mints the id that names one transfer end to end. The
// 128-bit random suffix makes pre-insert tokens safe too: those use row id 0
// because the token and full submission payload must be committed atomically
// before contacting the client.
//
// The row id alone would be enough to be unique here, but not to be
// unambiguous THERE: ids restart when a database is rebuilt from a backup,
// and a stale "t-42" in a media server's log would then point at a
// different download entirely. The random half makes a collision across
// rebuilds something you would have to be unlucky twice to see.
func newTransferID(downloadID int64) (string, error) {
	return ports.NewTransferToken(downloadID)
}

// addToClient sends the release, carrying its human name and transfer id when
// the client can hold them. The type assertion is the whole mechanism: clients
// that cannot tag a download are added exactly as before, and Monarr keeps the
// id on its own row so its trace is still threaded.
func addToClient(ctx context.Context, c ports.DownloadClient, downloadURL, category, name, transfer string, priority int, deliveryExpected, manualAdditional bool) (ports.Handle, error) {
	if configured, ok := c.(ports.ConfiguredAdder); ok {
		return configured.AddWithOptions(ctx, downloadURL, category, ports.AddOptions{
			Name: name, Transfer: transfer, Priority: priority, DeliveryExpected: deliveryExpected, ManualAdditional: manualAdditional,
		})
	}
	if tagger, ok := c.(ports.TaggedAdder); ok && transfer != "" {
		return tagger.AddTagged(ctx, downloadURL, category, name, transfer)
	}
	return c.Add(ctx, downloadURL, category)
}

// retryAutomaticSubmission replays only an exact persisted submission with a
// negotiated durable Runner token. The token makes this safe when the prior
// Add response was lost; clients without that contract remain visibly
// uncertain instead of receiving a possible duplicate.
func (s *Service) retryAutomaticSubmission(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig) error {
	if !automaticSubmissionRetryable(dl) || dl.Transfer == "" || dl.GenerationKey == "" {
		return fmt.Errorf("automatic submission cannot be safely replayed")
	}
	var payload manualSubmissionPayload
	if dl.ExecutionPayload == "" || json.Unmarshal([]byte(dl.ExecutionPayload), &payload) != nil || payload.URL == "" {
		return fmt.Errorf("persisted automatic submission payload is unavailable")
	}
	blocked, err := s.db.IsBlocklisted(ctx, dl.ReleaseTitle, dl.Indexer)
	if err != nil {
		return err
	}
	if blocked {
		_ = s.db.RejectDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "release became blocklisted before automatic retry")
		return fmt.Errorf("release is blocklisted; automatic submission was stopped")
	}
	if dl.SourceGUID != "" && dl.SourceIndexerID > 0 {
		_, blocked, err = s.db.ReleaseIdentityBlocklistReason(ctx, dl.MediaItemID, dl.SourceIndexerID, dl.SourceGUID)
		if err != nil {
			return err
		}
		if blocked {
			_ = s.db.RejectDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "source release became blocklisted before automatic retry")
			return fmt.Errorf("source release is blocklisted; automatic submission was stopped")
		}
	}
	client := s.newClient(cfg)
	if client == nil {
		return fmt.Errorf("download client is unavailable")
	}
	due, err := s.db.RunnerSubmissionRetryDue(ctx, dl.ID, dl.GenerationKey, time.Now())
	if err != nil {
		return err
	}
	if !due {
		return errSubmissionRetryNotDue
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	handle, err := addToClient(callCtx, client, payload.URL, payload.Category, dl.ReleaseTitle, dl.Transfer, payload.Priority, dl.DeliveryExpected, false)
	if err != nil {
		return err
	}
	if handle == "" {
		if correlator, ok := client.(ports.ExactTransferCorrelator); ok && correlator.SupportsExactTransferCorrelation() {
			return s.db.RecordHandlelessSubmission(context.WithoutCancel(ctx), dl.ID, dl.GenerationKey, dl.Transfer)
		}
		return fmt.Errorf("client accepted the request without a native handle or exact transfer correlation")
	}
	return s.db.RecordDownloadSubmission(context.WithoutCancel(ctx), dl.ID, dl.GenerationKey, dl.Transfer, string(handle))
}

func negotiateRunnerDelivery(ctx context.Context, c ports.DownloadClient) (bool, bool, error) {
	protocol, ok := c.(ports.DeliveryProtocol)
	if !ok {
		return false, false, nil
	}
	page, err := protocol.ListDeliveries(ctx, 0, 1)
	if err != nil {
		var status ports.HTTPStatusCoder
		if errors.As(err, &status) && status.HTTPStatusCode() == http.StatusNotFound {
			return false, false, nil
		}
		return false, false, fmt.Errorf("negotiate Runner delivery protocol: %w", err)
	}
	if page.ProtocolVersion != 1 {
		return false, false, fmt.Errorf("unsupported Runner delivery protocol version %d", page.ProtocolVersion)
	}
	idempotent := false
	if tokenProtocol, ok := c.(ports.RequestTokenIdempotency); ok {
		idempotent, err = tokenProtocol.SupportsRequestTokenIdempotency(ctx)
		if err != nil {
			return false, false, fmt.Errorf("negotiate Runner request-token idempotency: %w", err)
		}
	}
	return true, idempotent, nil
}

func (s *Service) downloadPriority(ctx context.Context, item domain.MediaItem, copyID int64) (int, error) {
	if item.DownloadPriorityOverride != nil {
		return *item.DownloadPriorityOverride, nil
	}
	if copyID == 0 {
		return item.DownloadPriority, nil
	}
	for _, copy := range item.Copies {
		if copy.ID != copyID {
			continue
		}
		profile, err := s.db.GetProfile(ctx, copy.QualityProfileID)
		if err != nil {
			return 0, err
		}
		return profile.DownloadPriority, nil
	}
	return 0, fmt.Errorf("%w: copy %d", ErrNotFound, copyID)
}

// Grab sends the release to the right client by protocol and records the
// queue row + history (blueprint §5.1 grab → import).
func (s *Service) Grab(ctx context.Context, req GrabRequest) (int64, error) {
	req.Intent = GrabIntentAutomatic
	return s.grab(ctx, req, false)
}

// GrabManual admits an independent, unmonitored additional copy. The API
// entry point is the only production caller; automatic planners use Grab.
func (s *Service) GrabManual(ctx context.Context, req GrabRequest) (int64, error) {
	if strings.TrimSpace(req.RequestID) == "" {
		return 0, fmt.Errorf("requestId is required for a manual additional copy")
	}
	req.Intent = GrabIntentManualAdditional
	return s.grab(ctx, req, true)
}

func (s *Service) existingManualRequest(ctx context.Context, requestID, fingerprint string) (int64, bool, error) {
	record, err := s.db.GetManualGrabRequest(ctx, requestID)
	if errors.Is(err, ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if record.RequestFingerprint != fingerprint {
		return 0, true, ErrRequestConflict
	}
	dl, err := s.db.GetDownload(ctx, record.DownloadID)
	if errors.Is(err, ErrNotFound) {
		return 0, true, sqlite.ErrManualGrabRequestRetained
	}
	if err != nil {
		return 0, true, err
	}
	if dl.GenerationKey != record.GenerationKey || dl.CopyID != record.CopyID || dl.RequestID != requestID || dl.RequestFingerprint != fingerprint {
		return 0, true, fmt.Errorf("manual request download generation no longer matches its durable request record")
	}
	return record.DownloadID, true, nil
}

func (s *Service) resumeManualRequest(ctx context.Context, id int64) (int64, error) {
	unlock := s.lockDownload(id)
	defer unlock()
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return id, err
	}
	if dl.Intent != GrabIntentManualAdditional || dl.RequestID == "" || dl.Transfer == "" {
		return id, fmt.Errorf("manual request is missing its durable intent or transfer token")
	}
	if dl.SubmissionPhase == "rejected" || dl.State == "failed" {
		return id, fmt.Errorf("manual request was not submitted: %s", dl.Error)
	}
	if dl.ParkedAt.UnixMilli() != 0 || dl.ParkedReason != "" || dl.Superseded {
		return id, fmt.Errorf("manual request was cancelled or superseded; it will not be replayed")
	}
	if (dl.SubmissionPhase == "submitted" && dl.Handle != "") || dl.State == "downloaded" || dl.State == "imported" || dl.State == "awaiting_import" || dl.State == "importing" {
		return id, nil
	}
	if dl.SubmissionPhase == "submitted" && dl.Handle == "" {
		if !dl.ExactTransferCorrelation {
			return id, fmt.Errorf("submitted manual request is missing its native handle and was not admitted with exact transfer correlation")
		}
		// qBittorrent acknowledges Add before its hash becomes visible. The
		// accepted token is already durable, so this path only adopts an exact
		// token match and otherwise keeps the successful request pending.
		cfg, cfgErr := s.db.GetDownloadClient(ctx, dl.ClientID)
		if cfgErr != nil {
			return id, cfgErr
		}
		statuses, statusErr := s.newClient(cfg).Statuses(ctx)
		if statusErr != nil {
			_ = s.db.ScheduleManualAdmissionRetry(context.WithoutCancel(ctx), id, time.Now(), "accepted request is waiting for exact transfer-token correlation: "+statusErr.Error())
			return id, nil
		}
		for _, st := range statuses {
			if dl.Transfer == "" || st.Transfer != dl.Transfer || st.Handle == "" {
				continue
			}
			if err := s.db.CommitManualSubmission(ctx, id, dl.GenerationKey, string(st.Handle), dl.Transfer); err != nil {
				return id, fmt.Errorf("persist reconciled manual submission: %w", err)
			}
			return id, nil
		}
		_ = s.db.ScheduleManualAdmissionRetry(context.WithoutCancel(ctx), id, time.Now(), "client accepted the request; waiting for exact transfer-token correlation")
		return id, nil
	}
	providerDeadline, deadlineErr := s.db.DownloadProviderRetryAfter(ctx, id)
	if deadlineErr != nil {
		return id, deadlineErr
	}
	if providerDeadline.After(time.Now()) {
		return id, fmt.Errorf("download client requested that this same manual submission wait until %s", providerDeadline.UTC().Format(time.RFC3339))
	}
	if dl.SubmissionPhase == "pending" && !dl.BlacklistOverride {
		blocked, blockErr := s.db.IsBlocklisted(ctx, dl.ReleaseTitle, dl.Indexer)
		if blockErr != nil {
			return id, blockErr
		}
		if dl.SourceGUID != "" && dl.SourceIndexerID != 0 {
			_, exactBlocked, exactErr := s.db.ReleaseIdentityBlocklistReason(ctx, dl.MediaItemID, dl.SourceIndexerID, dl.SourceGUID)
			if exactErr != nil {
				return id, exactErr
			}
			blocked = blocked || exactBlocked
		}
		if blocked {
			reason, _, _ := s.db.BlocklistReason(ctx, dl.ReleaseTitle, dl.Indexer)
			if reason == "" {
				reason = "release was blacklisted before submission"
			}
			if err := s.db.RejectDownloadSubmission(ctx, id, dl.GenerationKey, dl.Transfer, "manual request was blocked before submission: "+reason); err != nil {
				return id, err
			}
			s.advance(ctx, &dl, "failed", 0, reason, stepFailed, "manual request was blocked before submission")
			return id, fmt.Errorf("manual request was blocked before submission: %s", reason)
		}
	}
	var payload manualSubmissionPayload
	if err := json.Unmarshal([]byte(dl.ExecutionPayload), &payload); err != nil || payload.URL == "" {
		return id, fmt.Errorf("manual request has no durable client submission payload")
	}
	cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil {
		return id, err
	}
	client := s.newClient(cfg)
	if dl.SubmissionPhase == "submitting" || dl.SubmissionPhase == "uncertain" || dl.SubmissionPhase == "submitted" {
		if !dl.RequestTokenIdempotent {
			statuses, statusErr := client.Statuses(ctx)
			if statusErr != nil {
				return id, statusErr
			}
			for _, st := range statuses {
				matches := st.Transfer == dl.Transfer && dl.Transfer != ""
				if st.Transfer == "" && dl.Handle != "" && string(st.Handle) == dl.Handle {
					matches = true
				}
				if matches {
					if st.Handle == "" {
						return id, fmt.Errorf("manual request matched by transfer token but the client omitted its handle")
					}
					if err := s.db.CommitManualSubmission(ctx, id, dl.GenerationKey, string(st.Handle), dl.Transfer); err != nil {
						return id, fmt.Errorf("persist reconciled manual submission: %w", err)
					}
					return id, nil
				}
			}
			return id, fmt.Errorf("manual submission is uncertain; refusing to create a possible duplicate without request-token idempotency")
		}
	}
	return s.dispatchManualRequest(ctx, dl, cfg, client, payload)
}

func (s *Service) dispatchManualRequest(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, client ports.DownloadClient, payload manualSubmissionPayload) (int64, error) {
	if err := s.db.SetSubmissionPhase(ctx, dl.ID, "submitting"); err != nil {
		return dl.ID, err
	}
	manualStepPresent := false
	for _, entry := range dl.Handoff {
		if entry.Step == stepManualGrabQueued {
			manualStepPresent = true
			break
		}
	}
	if !manualStepPresent {
		dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{Step: stepManualGrabQueued, At: time.Now().UnixMilli(), Detail: "independent manual copy reserved"})
		s.advance(ctx, &dl, "grabbed", 0, "", stepGrabbed, "sent to "+clientLabel(cfg)+" as "+dl.Transfer)
	}
	handle, err := addToClient(ctx, client, payload.URL, payload.Category, dl.ReleaseTitle, dl.Transfer, payload.Priority, dl.DeliveryExpected, true)
	if err != nil {
		if permanentSubmissionRefusal(err) {
			if rejectErr := s.db.RejectDownloadSubmission(context.WithoutCancel(ctx), dl.ID, dl.GenerationKey, dl.Transfer, "client permanently rejected the manual request: "+err.Error()); rejectErr != nil {
				return dl.ID, fmt.Errorf("persist manual admission refusal: %w", rejectErr)
			}
			return dl.ID, err
		}
		_ = s.db.SetSubmissionPhase(context.WithoutCancel(ctx), dl.ID, "uncertain")
		_ = s.db.UpdateDownloadState(context.WithoutCancel(ctx), dl.ID, "grabbed", 0, "submission uncertain; reconciling before retry")
		_ = s.db.ScheduleManualAdmissionRetry(context.WithoutCancel(ctx), dl.ID, time.Now(), err.Error(), submissionRetryDeadline(err))
		return dl.ID, err
	}
	var commitErr error
	if handle != "" {
		commitErr = s.db.CommitManualSubmission(ctx, dl.ID, dl.GenerationKey, string(handle), dl.Transfer)
	} else if correlator, ok := client.(ports.ExactTransferCorrelator); ok && correlator.SupportsExactTransferCorrelation() {
		commitErr = s.db.RecordHandlelessSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer)
	} else {
		commitErr = fmt.Errorf("client accepted the request without a native handle or exact transfer correlation")
	}
	if commitErr != nil {
		_ = s.db.SetSubmissionPhase(context.WithoutCancel(ctx), dl.ID, "uncertain")
		_ = s.db.ScheduleManualAdmissionRetry(context.WithoutCancel(ctx), dl.ID, time.Now(), "accepted by client but Curator could not persist its acknowledgement: "+commitErr.Error())
		return dl.ID, fmt.Errorf("client accepted manual request but its acknowledgement could not be persisted: %w", commitErr)
	}
	if handle == "" {
		s.advance(ctx, &dl, "grabbed", 0, "client accepted the request; waiting for exact transfer-token correlation", stepAcceptedAwaitingCorrelation,
			"Add was accepted; Curator will bind only a client status carrying this exact transfer token")
	}
	_ = s.db.AddHistory(ctx, "grabbed", dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"indexer": dl.Indexer, "protocol": dl.Protocol, "manualAdditional": true})
	s.publish(ReleaseGrabbed{MediaItemID: dl.MediaItemID, Title: dl.ReleaseTitle, Indexer: dl.Indexer, Protocol: dl.Protocol})
	s.log.Info("manual additional copy grabbed", "release", dl.ReleaseTitle, "client", cfg.Name, "transfer", dl.Transfer)
	return dl.ID, nil
}

func (s *Service) grab(ctx context.Context, req GrabRequest, manual bool) (int64, error) {
	var fingerprint string
	if manual {
		var err error
		fingerprint, err = manualRequestFingerprint(req, req.CopyID)
		if err != nil {
			return 0, err
		}
		id, found, err := s.existingManualRequest(ctx, req.RequestID, fingerprint)
		if err != nil {
			return 0, err
		}
		if found {
			return s.resumeManualRequest(ctx, id)
		}
	}
	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return 0, err
	}
	p := parser.Parse(req.Title)
	evidence := domain.MatchEvidence{Version: 1, Method: "manual_override", Code: "identity_unresolved", Reason: "Manually grabbed without retained search evidence", OriginalTitle: req.Title, ParsedTitle: p.Title}
	if req.MatchEvidence != nil {
		evidence = *req.MatchEvidence
	} else if verified, guid, indexerID, ok := s.consumeCandidateToken(req); ok {
		req.SourceGUID, req.IndexerID = guid, indexerID
		if verified.Matched {
			evidence = verified
		} else {
			evidence = domain.MatchEvidence{Version: 1, Method: "manual_override", Code: "manual_override",
				Reason: "Manually grabbed after rejection: " + verified.Reason, OriginalTitle: req.Title, ParsedTitle: p.Title}
		}
	}
	dest := item.Path
	rootFolderID := item.RootFolderID
	profileID := item.QualityProfileID
	bookType := item.BookType
	if req.CopyID != 0 {
		cp, err := s.db.GetMediaCopy(ctx, item.ID, req.CopyID)
		if err != nil {
			return 0, err
		}
		if cp.Path != "" {
			dest = cp.Path
		}
		rootFolderID = cp.RootFolderID
		profileID = cp.QualityProfileID
		bookType = cp.BookType
	}
	if dest == "" {
		// Refuse before talking to the download client. Discovering there is
		// nowhere to put a title only after all of its bytes arrived is the
		// expensive end of this validation.
		return 0, ErrNoLibraryFolder
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
	priority, err := s.downloadPriority(ctx, item, req.CopyID)
	if err != nil {
		return 0, err
	}
	blocked, err := s.db.IsBlocklisted(ctx, req.Title, req.Indexer)
	if err != nil {
		return 0, err
	}
	identityReason, identityBlocked := "", false
	if req.SourceGUID != "" && req.IndexerID != 0 {
		identityReason, identityBlocked, err = s.db.ReleaseIdentityBlocklistReason(ctx, item.ID, req.IndexerID, req.SourceGUID)
		if err != nil {
			return 0, err
		}
	}
	blocked = blocked || identityBlocked
	blacklistOverride := false
	if blocked {
		reason, _, _ := s.db.BlocklistReason(ctx, req.Title, req.Indexer)
		if identityBlocked {
			reason = identityReason
		}
		if reason == "" {
			reason = "previously failed"
		}
		if !manual || !req.AllowBlacklisted {
			return 0, fmt.Errorf("%w: %s", ErrBlacklistConfirmationRequired, reason)
		}
		blacklistOverride = true
	}
	client := s.newClient(*cfg)
	deliveryExpected, requestTokenIdempotent, err := negotiateRunnerDelivery(ctx, client)
	if err != nil {
		return 0, err
	}
	exactTransferCorrelation := requestTokenIdempotent
	if correlator, ok := client.(ports.ExactTransferCorrelator); ok && correlator.SupportsExactTransferCorrelation() {
		exactTransferCorrelation = true
	}
	if deliveryExpected {
		s.rememberRunnerDelivery(cfg.ID)
	}

	// Already grabbed and still moving? Two automation passes that overlap,
	// a failure that re-searched instantly, and a person pressing the
	// button all arrive here, and only the first of them should spend
	// 90 GB of wire. Jobs 238/239 on nuc3 were the same NZB six minutes
	// apart, and neither client nor monarr said a word about it.
	if !manual {
		if existing := s.alreadyInFlight(ctx, req); existing != 0 {
			s.log.Info("grab: already in flight; not grabbing it again",
				"release", req.Title, "download", existing)
			return existing, nil
		}
	}

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
	if req.CopyID != 0 && !manual {
		base = fmt.Sprintf("%s:c%d", base, req.CopyID)
	}
	wants := []string{base}
	// Persist the exact wire payload for every admission, not only manual
	// additional copies. Automatic Runner submissions can lose their response
	// or the process can exit after the row commits but before Add; recovery
	// must replay this URL/category/priority with the same transfer token.
	submissionPayload := manualSubmissionPayload{URL: req.DownloadURL, Category: cfg.Category, Priority: priority}

	// The row goes in BEFORE the client is asked to take the download,
	// because its stable transfer token and exact submission payload have to
	// be on the request that creates the download — a client cannot be told
	// afterwards what to have called it. A client that then refuses the
	// add leaves a row that describes a download nobody has, so it is
	// removed again below; the alternative (add first, insert second) is
	// a download running in a client that Monarr has no record of, which
	// is the worse of the two failures by a distance.
	s.importTargetMu.Lock()
	allocationCtx, releaseStorageAllocation := s.lockStorageDecision(ctx)
	releaseAllocation := func() { releaseStorageAllocation(); s.importTargetMu.Unlock() }
	active, activeErr := s.db.ListDownloadReservations(allocationCtx)
	if activeErr != nil {
		releaseAllocation()
		return 0, activeErr
	}
	for _, row := range active {
		if manual {
			continue
		}
		overlap := row.MediaItemID == item.ID && row.CopyID == req.CopyID && wantablesOverlap(row, wants)
		if overlap && row.PlanID != 0 && row.SubmissionPhase == "pending" && req.MatchEvidence == nil {
			cancelled, e := s.closePendingForManual(allocationCtx, row)
			if e != nil {
				releaseAllocation()
				return 0, e
			}
			if cancelled {
				continue
			}
		}
		if normalizeRelease(row.ReleaseTitle) == normalizeRelease(req.Title) || overlap || heldWantOverlap(row, item.ID, req.CopyID, wants) {
			releaseAllocation()
			return row.ID, nil
		}
	}
	download := sqlite.Download{
		MediaItemID: item.ID, CopyID: req.CopyID, WantableIDs: wants, Season: req.Season,
		ReleaseTitle: req.Title, Indexer: req.Indexer, Protocol: req.Protocol,
		Quality: p.Quality, Size: req.Size, ClientID: cfg.ID,
		State: "grabbed", MatchEvidence: evidence, Intent: req.Intent,
		RequestID: req.RequestID, RequestFingerprint: fingerprint,
		BlacklistOverride: blacklistOverride, DeliveryExpected: deliveryExpected,
		RequestTokenIdempotent: requestTokenIdempotent, ExactTransferCorrelation: exactTransferCorrelation,
		ReplacementOperationID: req.ReplacementOperationID, SourceGUID: req.SourceGUID, SourceIndexerID: req.IndexerID,
	}
	encodedSubmission, _ := json.Marshal(submissionPayload)
	download.ExecutionPayload = string(encodedSubmission)
	if !manual {
		// Persist the exact token and payload in the same INSERT as the
		// submitting phase, before Add can reach Runner. A crash at any point
		// after this commit is recoverable as the same idempotent request.
		download.Transfer, err = newTransferID(0)
		if err != nil {
			releaseAllocation()
			return 0, err
		}
		download.SubmissionPhase = "submitting"
	}
	var inserted sqlite.Download
	if manual {
		claims, e := s.db.ListFolderClaims(allocationCtx)
		if e != nil {
			releaseAllocation()
			return 0, e
		}
		copyName, copyPath := manualCopyPath(dest, req.RequestID, claims)
		copy := domain.MediaCopy{
			MediaItemID: item.ID, Name: copyName, BookType: bookType,
			QualityProfileID: profileID, RootFolderID: rootFolderID,
			Path: copyPath, Monitored: false,
		}
		inserted, err = s.db.InsertManualAdditionalDownload(allocationCtx, copy, download, newTransferID)
	} else {
		inserted, err = s.db.InsertDownloadRecord(allocationCtx, download)
	}
	releaseAllocation()
	if err != nil {
		if manual {
			if id, found, lookupErr := s.existingManualRequest(context.WithoutCancel(ctx), req.RequestID, fingerprint); found || lookupErr != nil {
				return id, lookupErr
			}
		}
		return 0, err
	}
	id := inserted.ID
	req.CopyID = inserted.CopyID
	transfer := inserted.Transfer
	if !manual && (transfer == "" || inserted.SubmissionPhase != "submitting" || inserted.ExecutionPayload == "") {
		return id, fmt.Errorf("automatic submission intent was not durably committed")
	}
	// Open the handoff trace BEFORE the client is asked, so every step from
	// here is laid out. A client that finishes instantly (a cached torrent, a
	// test double) can push its completion before this goroutine gets back
	// from the add; the poller then read a trace with no first line, appended
	// "downloaded" to it, and its write erased the "grabbed" step written a
	// moment later. The id is in the trace's first line because that is
	// where someone starts reading when a transfer goes wrong.
	dl := sqlite.Download{ID: id, GenerationKey: inserted.GenerationKey, MediaItemID: item.ID, ReleaseTitle: req.Title,
		State: "grabbed", Transfer: transfer, Indexer: req.Indexer, Protocol: req.Protocol, ClientID: cfg.ID}
	if manual {
		dl.Intent = GrabIntentManualAdditional
		dl.RequestID = req.RequestID
		dl.RequestFingerprint = fingerprint
		dl.CopyID = inserted.CopyID
		dl.WantableIDs = inserted.WantableIDs
		dl.Season = req.Season
		dl.DeliveryExpected = inserted.DeliveryExpected
		return s.resumeManualRequest(ctx, id)
	}
	unlockDownload := s.lockDownload(id)
	defer unlockDownload()
	fresh, freshErr := s.db.GetDownload(ctx, id)
	if freshErr != nil || fresh.GenerationKey != inserted.GenerationKey || fresh.Transfer != transfer || !automaticSubmissionPending(fresh) {
		return id, fmt.Errorf("automatic submission was cancelled before client admission")
	}
	inserted = fresh
	s.advance(ctx, &dl, "grabbed", 0, "", stepGrabbed, "sent to "+clientLabel(*cfg)+" as "+transfer)
	handle, err := addToClient(
		ctx, client, req.DownloadURL, cfg.Category, req.Title, transfer, priority, inserted.DeliveryExpected, false,
	)
	if err != nil {
		if permanentSubmissionRefusal(err) {
			if rejectErr := s.db.RejectDownloadSubmission(context.WithoutCancel(ctx), id, inserted.GenerationKey, transfer, "client permanently rejected the exact submission: "+err.Error()); rejectErr != nil {
				return id, fmt.Errorf("persist automatic admission refusal: %w", rejectErr)
			}
			return id, err
		}
		_ = s.db.SetSubmissionPhase(context.WithoutCancel(ctx), id, "uncertain")
		_ = s.db.UpdateDownloadState(context.WithoutCancel(ctx), id, "grabbed", 0, "submission uncertain; reconciling before retry")
		_, _ = s.db.ScheduleRunnerSubmissionRetry(context.WithoutCancel(ctx), id, inserted.GenerationKey, time.Now(), err.Error(), submissionRetryDeadline(err))
		return id, err
	}
	var commitErr error
	if handle != "" {
		commitErr = s.db.RecordDownloadSubmission(ctx, id, inserted.GenerationKey, transfer, string(handle))
	} else if correlator, ok := client.(ports.ExactTransferCorrelator); ok && correlator.SupportsExactTransferCorrelation() {
		commitErr = s.db.RecordHandlelessSubmission(ctx, id, inserted.GenerationKey, transfer)
	} else {
		commitErr = fmt.Errorf("client accepted the request without a native handle or exact transfer correlation")
	}
	if commitErr != nil {
		_ = s.db.SetSubmissionPhase(context.WithoutCancel(ctx), id, "uncertain")
		_ = s.db.UpdateDownloadState(context.WithoutCancel(ctx), id, "grabbed", 0, "client accepted the request but its exact handle could not be committed; reconciling by transfer token")
		return id, fmt.Errorf("client accepted the request but submission receipt could not be saved: %w", commitErr)
	}
	if handle == "" {
		s.advance(ctx, &dl, "grabbed", 0, "client accepted the request; waiting for exact transfer-token correlation", stepAcceptedAwaitingCorrelation,
			"Add was accepted; Curator will bind only a client status carrying this exact transfer token")
	}
	_ = s.db.AddHistory(ctx, "grabbed", item.ID, req.Title,
		map[string]any{"indexer": req.Indexer, "protocol": req.Protocol, "downloadPriority": priority, "match": evidence})
	s.publish(ReleaseGrabbed{MediaItemID: item.ID, Title: req.Title,
		Indexer: req.Indexer, Protocol: req.Protocol})
	s.log.Info("grabbed", "title", req.Title, "client", cfg.Name, "download_priority", priority)
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
		client := s.newClient(cfg)
		// Durable Runner publications are discovered independently of the
		// client's ordinary history/status response. The optional capability
		// prevents legacy completion history from importing a native Runner job
		// before its exact immutable manifest is durably accepted.
		s.discoverRunnerDeliveries(ctx, cfg, client)
		var statuses []ports.DownloadStatus
		var err error
		complete := false
		if inventory, ok := client.(ports.CompleteInventory); ok {
			// Use one fresh inventory, rather than counting a second poll.
			statuses, complete, err = inventory.Inventory(ctx)
		} else {
			statuses, err = client.Statuses(ctx)
		}
		s.noteContact(cfg.ID, err)
		for _, dl := range byClient[cfg.ID] {
			if s.ImportRunning(dl.ID) {
				continue
			}
			s.observeUncertain(ctx, dl, cfg, statuses, err == nil && complete)
			if err != nil {
				s.resetStallObservation(ctx, dl)
			}
		}
		if err != nil {
			s.log.Warn("queue: client poll failed", "client", cfg.Name, "err", err)
		}
		// Receipt reads can outlive queue/history removal, but a large backlog
		// of missing rows must not serialize one HTTP timeout per download on
		// this control loop. Persist each row's next due time and admit at most
		// two exact-token lookups per client refresh. Due rows beyond the cap
		// remain eligible next sweep after earlier claims move forward.
		admissionPollAllowed := make(map[int64]bool)
		admissionPollCount := 0
		pollOrder, orderErr := s.db.RunnerAdmissionPollOrder(ctx, cfg.ID)
		if orderErr != nil {
			s.log.Warn("queue: could not order Runner receipt lookups", "client", cfg.Name, "err", orderErr)
		}
		pollRank := make(map[int64]int, len(pollOrder))
		for rank, id := range pollOrder {
			pollRank[id] = rank
		}
		pollRows := append([]sqlite.Download(nil), byClient[cfg.ID]...)
		sort.SliceStable(pollRows, func(i, j int) bool {
			ri, iok := pollRank[pollRows[i].ID]
			rj, jok := pollRank[pollRows[j].ID]
			if iok != jok {
				return iok
			}
			if !iok {
				return false
			}
			return ri < rj
		})
		for _, dl := range pollRows {
			if admissionPollCount >= 2 {
				break
			}
			if !dl.RequestTokenIdempotent || dl.GenerationKey == "" || s.ImportRunning(dl.ID) {
				continue
			}
			_, visible := matchStatus(dl, statuses)
			st, _ := matchStatus(dl, statuses)
			if visible && st.State != ports.StateFailed && st.State != ports.StateRemoved && st.State != ports.StateCompleted {
				continue
			}
			claimed, claimErr := s.db.ClaimRunnerAdmissionPoll(ctx, dl.ID, dl.GenerationKey, time.Now())
			if claimErr != nil {
				s.log.Warn("queue: could not claim Runner receipt lookup", "download", dl.ID, "err", claimErr)
				continue
			}
			if claimed {
				admissionPollAllowed[dl.ID] = true
				admissionPollCount++
			}
		}
		seen := make(map[int64]bool, len(byClient[cfg.ID]))
		for _, dl := range byClient[cfg.ID] {
			if err != nil {
				break
			}
			st, ok := matchStatus(dl, statuses)
			if !ok {
				if dl.ExactTransferCorrelation && dl.SubmissionPhase == "submitted" && dl.Handle == "" && dl.State == "grabbed" && time.Since(dl.UpdatedAt) >= manualAdmissionBlockedWarning {
					warning := "Accepted download has not appeared with its exact transfer token for at least 30 minutes; review the client before retrying."
					_ = s.db.SetExactTransferCorrelationWarning(ctx, dl.ID, dl.GenerationKey, dl.Transfer, warning)
				}
				continue // not visible yet (magnet resolving, etc.)
			}
			seen[dl.ID] = true
			if dl.RequestTokenIdempotent && (st.State == ports.StateFailed || st.State == ports.StateRemoved || st.State == ports.StateCompleted) && !admissionPollAllowed[dl.ID] {
				continue
			}
			// An importer owns this row's per-download lock for the whole
			// placement. Waiting for that lock here would put a multi-gigabyte
			// file copy back onto the queue-refresh goroutine, freezing every
			// client's contact clock and falsely reporting the client as
			// degraded. The import worker already owns the next state change;
			// this observation has nothing useful to add until it is done.
			if s.ImportRunning(dl.ID) {
				continue
			}
			pollCtx := ctx
			if admissionPollAllowed[dl.ID] {
				pollCtx = context.WithValue(ctx, runnerAdmissionPollPermitKey{}, dl.ID)
			}
			s.reconcileDownload(pollCtx, dl, cfg, st, "poll")
		}
		// Runner request receipts survive queue/history removal. Poll only
		// those exact-token admissions that vanished from ordinary inventory;
		// legacy clients retain their established reconciliation path.
		for _, dl := range byClient[cfg.ID] {
			if seen[dl.ID] || !dl.RequestTokenIdempotent || !admissionPollAllowed[dl.ID] || s.ImportRunning(dl.ID) {
				continue
			}
			pollCtx := context.WithValue(ctx, runnerAdmissionPollPermitKey{}, dl.ID)
			s.reconcileDownload(pollCtx, dl, cfg, ports.DownloadStatus{}, "admission")
		}
		if err != nil {
			continue
		}
		// Anything this client no longer mentions is not moving, whatever the
		// in-flight view still believes.
		//
		// The view is fed by observations, and an observation that never
		// arrives cannot end anything. Deleting a job in nzbd is exactly that
		// shape: the job stops being reported, so the last thing Monarr ever
		// heard about it was "downloading", and it sat on the panel forever
		// claiming to be in progress. A live view that can only ever be added
		// to is a leak with a progress bar.
		//
		// So the sweep reconciles rather than reacts. It is the safety net,
		// not the mechanism — the event stream ends these within a second
		// (see EventRemoved); this catches whatever the stream missed, and
		// covers clients that cannot push at all. Only runs when the client
		// actually answered, so an outage never mass-clears the panel.
		s.endUnseenTransfers(byClient[cfg.ID], seen)
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
	// Resolve Runner's network receipt before entering the global storage
	// decision lock. Per-download serialization prevents duplicate lookups;
	// generation/transfer are rechecked after the HTTP call below.
	observed := dl
	if fresh, err := s.db.GetDownload(ctx, dl.ID); err != nil {
		return
	} else {
		if fresh.GenerationKey != observed.GenerationKey || fresh.Transfer != observed.Transfer || fresh.ClientID != observed.ClientID {
			return
		}
		dl = fresh
	}
	if dl.RequestTokenIdempotent && (source == "admission" || st.State == ports.StateFailed || st.State == ports.StateRemoved || st.State == ports.StateCompleted) {
		if authoritative, hold := s.authoritativeRunnerObservation(ctx, dl, cfg, st); hold {
			return
		} else {
			st = authoritative
		}
	}
	if source == "admission" && st.State == "" {
		// The exact Runner receipt still describes a nonterminal request. It
		// settles no state until the job reappears or its durable outcome ends.
		return
	}
	dispatchCtx := ctx
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	storageReleased := false
	releaseStorageBeforeDispatch := func() {
		if !storageReleased {
			releaseStorage()
			storageReleased = true
		}
		// Never pass an inactive storage lease into queued or inline work.
		ctx = dispatchCtx
	}
	defer releaseStorageBeforeDispatch()

	// Re-read under the storage lock. The download identity must still be the
	// exact generation whose receipt was just checked.
	if fresh, err := s.db.GetDownload(ctx, dl.ID); err != nil {
		return
	} else {
		if fresh.GenerationKey != dl.GenerationKey || fresh.Transfer != dl.Transfer || fresh.ClientID != dl.ClientID {
			return
		}
		dl = fresh
	}

	if strings.HasPrefix(dl.ParkedReason, "operator cancellation") {
		return
	}
	if dl.Superseded {
		_ = s.db.UpdateDownloadState(ctx, dl.ID, dl.State, st.Progress, "Possible duplicate: late original submission cannot import automatically")
		return
	}
	if dl.SubmissionPhase == "uncertain" || dl.SubmissionPhase == "submitting" {
		_ = s.db.SetSubmissionPhase(ctx, dl.ID, "submitted")
	}
	if dl.Handle == "" && st.Handle != "" {
		var bindErr error
		if dl.Transfer != "" && st.Transfer == dl.Transfer {
			if dl.Intent == GrabIntentManualAdditional {
				bindErr = s.db.CommitManualSubmission(ctx, dl.ID, dl.GenerationKey, string(st.Handle), dl.Transfer)
			} else {
				bindErr = s.db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, string(st.Handle))
			}
		} else {
			bindErr = s.db.SetDownloadHandle(ctx, dl.ID, string(st.Handle), dl.Transfer)
		}
		if bindErr != nil {
			return
		}
		dl.Handle = string(st.Handle)
	}

	if !s.acceptDownloadControl(ctx, &dl, &st) {
		return
	}

	s.observeTorrentStall(ctx, dl, cfg, st)
	if fresh, e := s.db.GetDownload(ctx, dl.ID); e == nil && fresh.State == "failed" {
		return
	}

	// A completion is not actionable without the location of its payload.
	// A download-client adapter can misclassify a download-phase completion as
	// payload readiness, or receive a terminal response before its path. Never
	// persist that empty path and never let the importer turn
	// it into the misleading "payload missing: stat : no such file" failure.
	// Keep the download active until a later observation supplies the path.
	if st.State == ports.StateCompleted && strings.TrimSpace(st.SavePath) == "" {
		st.State = ports.StateDownloading
		st.Progress = 1
		if st.Message == "" {
			st.Message = "download client reports completion; waiting for payload path"
		}
	}

	// Progress can be stale by the time it acquires this row's lock: a final
	// event may have already crossed the import boundary while a preceding
	// tick was waiting. Never let queued/downloading observations move durable
	// handoff or terminal states backward.
	if st.State == ports.StateQueued || st.State == ports.StateDownloading {
		switch dl.State {
		case "awaiting_import", "downloaded", "importing", "imported", "failed":
			return
		}
	}

	// The in-flight view is maintained here, not at the call sites.
	//
	// It used to be updated only from the poll, so a download that changed
	// state via the event stream — the fast path, the one that exists to make
	// this immediate — never had its downloading row retired. Deleting a job
	// in nzbd left it on the panel indefinitely. Every observation of a
	// download reaches this function from both channels, so this is the only
	// place that cannot be forgotten by one of them.
	s.watchDownloading(dl, st, st.Stage)

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
		// Failed post-processing can leave a partial, recoverable payload.
		// Preserve its association for inventory and manual import without
		// treating the client's failure as permission to auto-import it.
		if strings.TrimSpace(st.SavePath) != "" {
			dl.SavePath = st.SavePath
			dl.ImportPath = ports.MapRemotePath(cfg.PathMappings, st.SavePath)
		}
		// A client-reported failure is normally a bad release: blocklist it
		// and search a replacement. Unless the client says the release is
		// not to blame — an operator deleting the job is not a reason to
		// ban the release forever.
		releaseStorageBeforeDispatch()
		s.handleFailure(ctx, dl, st.Progress, st.Message, st.Blameless)
	case ports.StateRemoved:
		if dl.State == "imported" || dl.State == "failed" {
			return
		}
		releaseStorageBeforeDispatch()
		s.handleRemoved(ctx, dl, st.Progress, st.Message, source)
	case ports.StateCompleted:
		mappedPath := ports.MapRemotePath(cfg.PathMappings, st.SavePath)
		legacyReview, reviewErr := s.db.LegacyCleanupReview(ctx, dl.GenerationKey, mappedPath)
		if reviewErr != nil || legacyReview {
			if reviewErr != nil {
				s.log.Warn("legacy completion review lookup failed; holding import", "download", dl.ID, "err", reviewErr)
			}
			if dl.State == "grabbed" || dl.State == "downloading" {
				s.recordDownloaded(ctx, &dl, cfg, st, source)
			} else {
				dl.SavePath, dl.ImportPath = st.SavePath, mappedPath
			}
			if dl.State != "awaiting_import" {
				s.advance(ctx, &dl, "awaiting_import", 1, "", stepAwaiting,
					"held for explicit review of pre-existing download custody")
			}
			return
		}
		if dl.DeliveryExpected && s.runnerDeliveryEnabled(ctx, cfg) {
			return
		}
		// 'downloaded' and 'importing' are NOT terminal, but they mean
		// someone is already on it; only 'grabbed'/'downloading' should
		// start an import from here.
		switch dl.State {
		case "imported", "awaiting_import", "downloaded", "failed":
			return
		case "importing":
			if s.ImportRunning(dl.ID) {
				return
			}
			// "importing" is durable; the worker and its registry are not. A
			// process restart used to strand this row forever because every
			// later client observation treated the word itself as proof that a
			// worker still existed. Record the recovery and hand it to a worker.
			s.advance(ctx, &dl, "downloaded", dl.Progress, "", stepImportRecovered,
				"previous import was interrupted; restarting from the payload")
			releaseStorageBeforeDispatch()
			s.enqueueImport(ctx, dl, cfg)
			return
		}
		releaseStorageBeforeDispatch()
		s.onDownloaded(ctx, dl, cfg, st, source)
	}
}

// authoritativeRunnerObservation consults Runner's durable request receipt for
// jobs admitted with request-token idempotency. Native queue/history status can
// disappear or collapse a fetch failure into a generic failed state; only the
// exact transfer token can settle that observation.
func (s *Service) authoritativeRunnerObservation(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, st ports.DownloadStatus) (ports.DownloadStatus, bool) {
	client, ok := s.newClient(cfg).(ports.RequestAdmissionProtocol)
	if !ok {
		return st, false
	}
	if dl.Transfer == "" {
		_ = s.db.ParkDownload(ctx, dl.ID, "Runner outcome requires the persisted transfer token; automatic handling held")
		return st, true
	}
	if permit, ok := ctx.Value(runnerAdmissionPollPermitKey{}).(int64); !ok || permit != dl.ID {
		claimed, claimErr := s.db.ClaimRunnerAdmissionPoll(ctx, dl.ID, dl.GenerationKey, time.Now())
		if claimErr != nil {
			s.log.Warn("could not claim Runner admission receipt lookup", "download", dl.ID, "err", claimErr)
			return st, true
		}
		if !claimed {
			return st, true
		}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	admission, found, err := client.GetRequestAdmission(lookupCtx, dl.Transfer)
	cancel()
	if recordErr := s.db.RecordRunnerAdmissionPollResult(ctx, dl.ID, dl.GenerationKey, found, err, time.Now()); recordErr != nil {
		s.log.Warn("could not record Runner admission receipt lookup", "download", dl.ID, "err", recordErr)
	}
	if err != nil {
		if dl.Intent != GrabIntentManualAdditional && dl.RequestTokenIdempotent &&
			(dl.SubmissionPhase == "submitting" || dl.SubmissionPhase == "uncertain") {
			_ = s.db.UpdateDownloadState(ctx, dl.ID, dl.State, st.Progress, "Runner admission lookup failed; exact automatic submission replay is queued")
			return st, true
		}
		if dl.Intent == GrabIntentManualAdditional || st.State == ports.StateFailed || st.State == ports.StateRemoved {
			_ = s.db.UpdateDownloadState(ctx, dl.ID, dl.State, st.Progress, "waiting for authoritative Runner outcome: "+err.Error())
			return st, true
		}
		return st, false
	}
	if !found {
		if dl.Intent != GrabIntentManualAdditional && dl.RequestTokenIdempotent &&
			(dl.SubmissionPhase == "submitting" || dl.SubmissionPhase == "uncertain") {
			_ = s.db.UpdateDownloadState(ctx, dl.ID, dl.State, st.Progress, "Runner has no admission receipt yet; exact automatic submission replay is queued")
			return st, true
		}
		if dl.Intent == GrabIntentManualAdditional || st.State == ports.StateFailed || st.State == ports.StateRemoved {
			_ = s.db.UpdateDownloadState(ctx, dl.ID, dl.State, st.Progress, "waiting for authoritative Runner request admission receipt")
			return st, true
		}
		return st, false
	}
	// The user may have removed the Activity row or replaced its request
	// generation while the network lookup was in flight. A receipt never
	// authorizes state changes against a stale row.
	fresh, freshErr := s.db.GetDownload(ctx, dl.ID)
	if freshErr != nil || fresh.GenerationKey != dl.GenerationKey || fresh.Transfer != dl.Transfer || fresh.ClientID != dl.ClientID {
		return st, true
	}
	dl = fresh
	if admission.ProtocolVersion != 1 || admission.RequestToken != dl.Transfer {
		_ = s.db.ParkDownload(ctx, dl.ID, "Runner admission identity mismatch; automatic handling held for review")
		return st, true
	}
	if err := s.db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer,
		admission.RunnerInstanceID, admission.Generation); err != nil {
		_ = s.db.ParkDownload(ctx, dl.ID, "Runner installation or admission generation changed; automatic handling held for review")
		return st, true
	}
	if admission.JobID != "" && dl.Handle != "" && admission.JobID != dl.Handle {
		_ = s.db.ParkDownload(ctx, dl.ID, "Runner admission job handle conflicts with the persisted download; review required")
		return st, true
	}
	if admission.JobID == "" {
		_ = s.db.ParkDownload(ctx, dl.ID, "Runner admission has no exact job handle; review required")
		return st, true
	}
	if dl.Handle == "" {
		if err := s.db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, admission.JobID); err != nil {
			_ = s.db.ParkDownload(ctx, dl.ID, "Runner job handle could not be committed to the exact download generation; review required")
			return st, true
		}
		dl.Handle = admission.JobID
	}
	if admission.Outcome == "failed" || admission.Outcome == "deleted" {
		if admission.CustodyState == "curator" && admission.PublicationPath != "" && dl.DeliveryExpected {
			// Delivery discovery/import owns the retained verified publication.
			// Do not fail the whole pack or trigger a fresh acquisition here.
			return st, true
		}
		if admission.CustodyState == "curator" {
			_ = s.db.ParkDownload(ctx, dl.ID, "Runner transferred payload custody without an expected delivery protocol; explicit review required")
			return st, true
		}
		if admission.Outcome == "failed" && dl.Intent != GrabIntentManualAdditional &&
			runnerFailureNeedsCleanupBeforeReplacement(admission) {
			reason := "Runner retains source files or Keep was selected; automatic replacement is held"
			if admission.FailureStage != "" {
				reason += " at " + admission.FailureStage
			}
			if admission.FailureCause != "" {
				reason += ": " + admission.FailureCause
			}
			_ = s.db.UpdateDownloadState(ctx, dl.ID, "awaiting_import", st.Progress, reason+"; replacement held until cleanup is settled")
			return st, true
		}
		if runnerDeletedNeedsCustodyReview(admission) {
			_ = s.db.ParkDownload(ctx, dl.ID, "Runner queue entry is gone but source custody is unresolved; explicit review required")
			return st, true
		}
		st.State = ports.StateFailed
		if admission.Outcome == "deleted" && admission.CustodyState == "deleted_by_runner" {
			st.State = ports.StateRemoved
		}
		st.Message = strings.TrimSpace(strings.Join([]string{admission.TerminalOutcome, admission.FailureStage, admission.FailureCause,
			"custody=" + admission.CustodyState, "sourceCleanup=" + admission.SourceCleanupStatus}, ": "))
		if st.Message == "" {
			st.Message = "Runner request ended: " + admission.Outcome
		}
		return st, false
	}
	if admission.Outcome == "completed" && admission.PublicationPath != "" {
		if dl.DeliveryExpected {
			return st, true
		}
		st.State = ports.StateCompleted
		st.Handle = ports.Handle(admission.JobID)
		st.Transfer = admission.RequestToken
		st.SavePath = admission.PublicationPath
		return st, false
	}
	if st.State == ports.StateFailed || st.State == ports.StateRemoved {
		_ = s.db.UpdateDownloadState(ctx, dl.ID, dl.State, st.Progress, "waiting for Runner terminal outcome; reported state is "+admission.Outcome)
		return st, true
	}
	return st, false
}

// runnerFailureNeedsCleanupBeforeReplacement only blocks an automatic retry
// while Runner still owns unresolved bytes. An explicit Keep is a settled
// custody decision: preserve those bytes and continue with the selected
// replacement, rather than requiring the user to delete something they chose
// to keep. A manual additional-copy attempt has its own terminal policy.
func runnerFailureNeedsCleanupBeforeReplacement(admission ports.RequestAdmission) bool {
	if admission.KeepSourceOnFailure || admission.CustodyState == "kept_by_user" {
		return false
	}
	return admission.CustodyState == "runner_owned" && admission.SourceCleanupStatus != "removed"
}

func runnerDeletedNeedsCustodyReview(admission ports.RequestAdmission) bool {
	return admission.Outcome == "deleted" && admission.CustodyState != "deleted_by_runner" &&
		!admission.KeepSourceOnFailure && admission.CustodyState != "kept_by_user"
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
// handleRemoved retires a download somebody deleted in the client.
//
// A deletion is an instruction, and this is the whole difference between it
// and a failure: no blocklist, and no automatic re-search. handleFailure runs
// one, which is correct when a release breaks and actively hostile when the
// operator has just said they do not want this one — delete a job in nzbd,
// and seconds later Monarr has grabbed another copy of the same film. Doing
// that once is confusing; doing it three times, which is what the re-search
// loop produces if you keep deleting, looks like the applications are
// fighting each other. They were.
//
// The want stays wanted. Nothing here touches monitoring, so the next
// deliberate search still finds it — the difference is that the search is
// yours to start.
func (s *Service) handleRemoved(ctx context.Context, dl sqlite.Download, progress float64, reason, source string) {
	if reason == "" {
		reason = "removed in the download client"
	}
	if dl.PlanID != 0 {
		// Persist the operator fence; failure fallback and automatic retry must
		// never reinterpret a deliberate removal as a failed release.
		_ = s.db.ParkDownload(ctx, dl.ID, "operator cancellation; deliberate Search required")
		_, _ = s.db.W.ExecContext(ctx, `UPDATE acquisition_plans SET state='cancel_requested' WHERE id=? AND state IN ('admitted','dispatching','active')`, dl.PlanID)
	}
	s.advance(ctx, &dl, "failed", progress, reason, stepFailed, traced(reason, source))
	_ = s.db.AddHistory(ctx, "failed", dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"reason": reason, "removed": true})
	s.log.Info("download removed in the client; not blocklisted and not replaced",
		"release", dl.ReleaseTitle, "reason", reason, "source", source)
	s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: reason})
}

// endUnseenTransfers retires in-flight rows for downloads the client did not
// mention in the sweep it just answered.
//
// `seen` holds the ids matched to a status this pass; anything active but
// absent is no longer moving, so its downloading row goes. Ending a stage
// that was never begun is a no-op, so this needs no membership check.
func (s *Service) endUnseenTransfers(active []sqlite.Download, seen map[int64]bool) {
	if s.registry == nil {
		return
	}
	for _, dl := range active {
		if seen[dl.ID] {
			continue
		}
		s.registry.Begin(transfers.Transfer{
			DownloadID: dl.ID, Stage: transfers.StageDownloading,
		}).End()
	}
}

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

func (s *Service) lockDeliveryAcknowledgement(receiptID int64) func() {
	s.deliveryAckMu.Lock()
	if s.deliveryAckLocks == nil {
		s.deliveryAckLocks = map[int64]*sync.Mutex{}
	}
	m, ok := s.deliveryAckLocks[receiptID]
	if !ok {
		m = &sync.Mutex{}
		s.deliveryAckLocks[receiptID] = m
	}
	s.deliveryAckMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Service) lockDeliverySourceControl(receiptID int64) func() {
	s.deliverySourceMu.Lock()
	if s.deliverySourceLocks == nil {
		s.deliverySourceLocks = map[int64]*sync.Mutex{}
	}
	m, ok := s.deliverySourceLocks[receiptID]
	if !ok {
		m = &sync.Mutex{}
		s.deliverySourceLocks[receiptID] = m
	}
	s.deliverySourceMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Service) tryLockDeliverySourceControl(receiptID int64) (func(), bool) {
	s.deliverySourceMu.Lock()
	if s.deliverySourceLocks == nil {
		s.deliverySourceLocks = map[int64]*sync.Mutex{}
	}
	m, ok := s.deliverySourceLocks[receiptID]
	if !ok {
		m = &sync.Mutex{}
		s.deliverySourceLocks[receiptID] = m
	}
	s.deliverySourceMu.Unlock()
	if !m.TryLock() {
		return nil, false
	}
	return m.Unlock, true
}

// handleFailure marks a download failed, blocklists the release so it is
// never re-grabbed, and immediately re-searches for the affected wantables
// (blueprint §5.1 "failed-download handling").
func (s *Service) handleFailure(ctx context.Context, dl sqlite.Download, progress float64, reason string, blameless bool) {
	s.advance(ctx, &dl, "failed", progress, reason, stepFailed, "download failed: "+reason)
	_ = s.db.AddHistory(ctx, "failed", dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"reason": reason})
	if dl.SubmissionPhase == "rejected" {
		// A definite client refusal (including an immutable-token conflict) did
		// not admit this request. Do not turn the refusal into a release
		// blacklist or launch a replacement behind the user's back.
		s.log.Warn("client permanently rejected the submission; no replacement was started", "release", dl.ReleaseTitle, "reason", reason)
		s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: reason})
		return
	}
	if blameless {
		// The download stopped, but not because the release is bad. Banning
		// it would burn a good copy and send the re-search after a worse one.
		s.log.Warn("download failed; NOT blocklisted (the release is not at fault)",
			"release", dl.ReleaseTitle, "reason", reason)
	} else {
		var err error
		if dl.SourceGUID != "" && dl.SourceIndexerID != 0 {
			err = s.db.AddReleaseIdentityBlocklist(ctx, dl.MediaItemID, dl.SourceIndexerID, dl.SourceGUID, reason)
		} else {
			// Legacy clients may not expose stable release identity. Keep their
			// existing title/indexer blocklist semantics explicit at this edge.
			err = s.db.AddBlocklist(ctx, dl.MediaItemID, dl.ReleaseTitle, dl.Indexer, reason)
		}
		if err != nil {
			s.log.Warn("blocklist: insert failed", "release", dl.ReleaseTitle, "err", err)
		} else {
			s.log.Warn("download failed; blocklisted", "release", dl.ReleaseTitle, "reason", reason)
		}
	}
	s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: reason})
	if dl.Intent == GrabIntentManualAdditional {
		// A manual additional copy is a deliberate one-shot acquisition. A
		// permanent failure stays visible and blocklisted, but never starts an
		// automatic replacement behind the user's back.
		return
	}
	if dl.ReplacementOperationID != "" {
		// The reviewed operation owns the selected copy/episode scope. Its
		// worker waits for durable payload cleanup before choosing another
		// release; a global planner search here would lose that scope.
		if err := s.db.TransitionReplacementOperation(ctx, dl.ReplacementOperationID, "replacing", "replacement attempt failed; waiting for payload cleanup before retry"); err != nil {
			s.log.Warn("replacement operation retry scheduling failed", "operation", dl.ReplacementOperationID, "err", err)
		}
		return
	}

	if item, e := s.db.GetMediaItemFull(ctx, dl.MediaItemID); e == nil && item.Kind == domain.KindSeries {
		if dl.PlanID != 0 {
			_, _ = s.db.W.ExecContext(ctx, `UPDATE acquisition_plans SET replan_pending=1 WHERE id=?`, dl.PlanID)
		}
		_ = s.enqueueSeason(ctx, dl.MediaItemID, dl.CopyID, dl.Season, "failed_acquisition", true)
		return
	}
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
		// Bounded. Five grabs of one movie in ten hours is not persistence,
		// it is a loop with a budget, and the budget is the operator's
		// bandwidth and disk. Once the window's grabs are spent the want
		// stays wanted — a person can still grab by hand — and it says so.
		if capped, spent := s.regrabCapped(ctx, w); capped {
			s.log.Warn("re-search stopped: too many failed grabs for this want recently",
				"wantable", idStr, "failed", spent, "window", reGrabWindow)
			_ = s.db.AddHistory(ctx, HistoryRegrabCapped, dl.MediaItemID, dl.ReleaseTitle,
				map[string]any{"wantable": idStr, "failed": spent,
					"window_hours": int(reGrabWindow.Hours())})
			continue
		}
		if _, err := s.searchAndGrabBest(ctx, w, enabled); err != nil {
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
		kind = "movie"
	} else if n, _ = fmt.Sscanf(id, "book:%d", &a); n == 1 {
		kind = "book"
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
	if (kind == "movie" && item.Kind != domain.KindMovie) ||
		(kind == "book" && item.Kind != domain.KindBook) ||
		((kind == "episode" || kind == "season") && item.Kind != domain.KindSeries) {
		return nil, fmt.Errorf("wantable %q does not match media kind %q: %w", id, item.Kind, ErrNotFound)
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
	case "movie", "book":
		return s.targetCopy(ctx, item, 0, 0, cp)
	default:
		return nil, fmt.Errorf("unparseable wantable id %q", id)
	}
}

func matchStatus(dl sqlite.Download, statuses []ports.DownloadStatus) (ports.DownloadStatus, bool) {
	if dl.ExactTransferCorrelation || dl.RequestTokenIdempotent || dl.DeliveryExpected {
		var match ports.DownloadStatus
		count := 0
		for _, st := range statuses {
			if dl.Transfer != "" && st.Transfer == dl.Transfer {
				if dl.Handle != "" && st.Handle != "" && string(st.Handle) != dl.Handle {
					return ports.DownloadStatus{}, false
				}
				match = st
				count++
			}
		}
		return match, count == 1
	}
	if dl.Intent == GrabIntentManualAdditional {
		for _, st := range statuses {
			if st.Transfer != "" {
				if st.Transfer == dl.Transfer && dl.Transfer != "" {
					return st, true
				}
				continue
			}
			if dl.Handle != "" && string(st.Handle) == dl.Handle {
				return st, true
			}
		}
		return ports.DownloadStatus{}, false
	}
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
	var matched ports.DownloadStatus
	count := 0
	for _, st := range statuses {
		if norm(st.Name) == want {
			matched = st
			count++
		}
	}
	if count == 1 {
		return matched, true
	}
	return ports.DownloadStatus{}, false
}

// History returns the recorded per-release events, newest first: grabbed,
// the client's outcome, the import result, and every stop in between.
//
// This existed and was reachable from nothing. A pipeline you cannot see is
// a pipeline that loops in the dark.
func (s *Service) History(ctx context.Context) ([]sqlite.HistoryEvent, error) {
	return s.db.ListHistory(ctx)
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
	if dl.PlanID != 0 || dl.SubmissionPhase == "uncertain" || dl.SubmissionPhase == "submitting" || dl.SubmissionPhase == "submitted" {
		unlock := s.lockDownload(id)
		defer unlock()
		dl, err = s.db.GetDownload(ctx, id)
		if err != nil {
			return err
		}
		if dl.SubmissionPhase == "pending" {
			_, err = s.db.W.ExecContext(ctx, `UPDATE downloads SET state='failed',submission_phase='rejected',superseded=1,wantables='[]',reserved_episodes='[]',error='operator cancelled unsubmitted intent' WHERE id=? AND submission_phase='pending'`, id)
		} else {
			if fromClient && dl.Handle != "" {
				cfg, e := s.db.GetDownloadClient(ctx, dl.ClientID)
				if e != nil {
					return e
				}
				client := s.newClient(cfg)
				if e = client.Remove(ctx, ports.Handle(dl.Handle), false); e != nil {
					return e
				}
				statuses, e := client.Statuses(ctx)
				if e != nil {
					return e
				}
				if _, present := matchStatus(dl, statuses); present {
					return fmt.Errorf("client removal awaiting confirmation")
				}
			}
			err = s.db.ParkDownload(ctx, id, "operator cancellation; custody retained for review")
		}
		if err != nil {
			return err
		}
		// Removal from Activity is a view dismissal. Accepted/uncertain work
		// keeps its durable row for transfer reconciliation and cleanup, but it
		// should no longer appear in the recent queue.
		result, err := s.db.W.ExecContext(ctx, `UPDATE downloads SET activity_dismissed=1, updated_at=? WHERE id=? AND generation_key=?`, time.Now().UnixMilli(), id, dl.GenerationKey)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return fmt.Errorf("download generation changed while dismissing Activity")
		}
		if dl.PlanID != 0 {
			_, err = s.db.W.ExecContext(ctx, `UPDATE acquisition_plans SET state='cancel_requested' WHERE id=? AND state IN ('admitted','dispatching','active')`, dl.PlanID)
		}
		return err
	}
	if fromClient && dl.Handle != "" {
		if cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID); err == nil {
			_ = s.newClient(cfg).Remove(ctx, ports.Handle(dl.Handle), false)
		}
	}
	return s.db.DeleteDownload(ctx, id)
}
