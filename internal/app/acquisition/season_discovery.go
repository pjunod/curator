package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/acquisitionplan"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/format"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

const SeasonSearchJobKind = "acquisition.season"

type discoveryScope struct {
	IndexerID int64
	EpisodeID int64
	Queries   []domain.SearchQuery
	Query     int
	Offset    int
	Pages     int
	Complete  bool
	Reasons   []string
	Failures  int
	RetryAt   time.Time
	Degraded  bool
}
type seasonCheckpoint struct {
	Fingerprint        string           `json:"fingerprint"`
	Deep               bool             `json:"deep"`
	Version            int              `json:"version"`
	ItemID             int64            `json:"itemId"`
	CopyID             int64            `json:"copyId"`
	Season             int              `json:"season"`
	Trigger            string           `json:"trigger"`
	OriginalDownloadID int64            `json:"originalDownloadId,omitempty"`
	Revision           int64            `json:"revision"`
	Started            time.Time        `json:"started"`
	Deadline           time.Time        `json:"deadline"`
	IntrinsicPartial   bool             `json:"intrinsicPartial"`
	SchedulingDeferred bool             `json:"schedulingDeferred"`
	Scopes             []discoveryScope `json:"scopes"`
	Releases           []ports.Release  `json:"releases"`
	Stage              string           `json:"stage"`
	CooldownUntil      time.Time        `json:"cooldownUntil"`
}

func seasonJobKey(item, copy int64, season int) string {
	return fmt.Sprintf("season:%d:%d:%d", item, copy, season)
}
func (s *Service) enqueueSeason(ctx context.Context, item, copy int64, season int, trigger string, missing bool) error {
	p := seasonCheckpoint{Version: 1, ItemID: item, CopyID: copy, Season: season, Trigger: trigger}
	_, pack, profile, targets, _, e := s.seasonSnapshot(ctx, p)
	if e != nil {
		return e
	}
	if len(targets) == 0 {
		return nil
	}
	p.Fingerprint = discoveryFingerprint(pack, profile, targets)
	var previous string
	if e = s.db.R.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE kind=? AND dedupe_key=? AND state='done' ORDER BY id DESC LIMIT 1`, SeasonSearchJobKind, seasonJobKey(item, copy, season)).Scan(&previous); e == nil {
		var old seasonCheckpoint
		if json.Unmarshal([]byte(previous), &old) == nil && old.Fingerprint == p.Fingerprint {
			if time.Now().Before(old.CooldownUntil) && trigger != "rss_improvement" {
				return nil
			}
			p.Deep = !old.Started.IsZero() && time.Since(old.Started) >= 7*24*time.Hour
		}
	}
	raw, _ := json.Marshal(p)
	priority := int64(110)
	if missing {
		priority = 50
	}
	_, err := s.db.EnqueueJob(ctx, domain.Job{Kind: SeasonSearchJobKind, Payload: string(raw), DedupeKey: seasonJobKey(item, copy, season), Priority: priority, MaxAttempts: 3})
	if errors.Is(err, sqlite.ErrDuplicateJob) {
		return nil
	}
	return err
}
func RegisterSeasonSearchJobs[H ~func(context.Context, domain.Job) error](s *Service, register func(string, H) error) error {
	return register(SeasonSearchJobKind, H(s.handleSeasonSearch))
}
func (s *Service) seasonSnapshot(ctx context.Context, p seasonCheckpoint) (domain.MediaItem, domain.SeasonWantable, quality.Profile, []domain.EpisodeWantable, bool, error) {
	item, err := s.db.GetMediaItemFull(ctx, p.ItemID)
	if err != nil {
		return item, domain.SeasonWantable{}, quality.Profile{}, nil, false, err
	}
	var cp *domain.MediaCopy
	if p.CopyID != 0 {
		for i := range item.Copies {
			if item.Copies[i].ID == p.CopyID {
				cp = &item.Copies[i]
				break
			}
		}
		if cp == nil || !cp.Monitored {
			return item, domain.SeasonWantable{}, quality.Profile{}, nil, false, fmt.Errorf("copy unavailable")
		}
	}
	w, err := s.targetCopy(ctx, item, p.Season, 0, cp)
	if err != nil {
		return item, domain.SeasonWantable{}, quality.Profile{}, nil, false, err
	}
	pack, ok := w.(domain.SeasonWantable)
	if !ok {
		return item, pack, quality.Profile{}, nil, false, fmt.Errorf("not a series season")
	}
	profile, err := s.db.GetProfile(ctx, pack.ProfileID())
	if err != nil {
		return item, pack, profile, nil, false, err
	}
	eligiblePack := p.Season > 0 && pack.Monitored() && len(pack.Episodes) > 1
	today := time.Now().UTC().Format(time.DateOnly)
	var targets []domain.EpisodeWantable
	aired := map[int64]bool{}
	for _, season := range item.Seasons {
		if season.Number != p.Season {
			continue
		}
		for _, e := range season.Episodes {
			if e.Monitored && e.AirDate != "" && e.AirDate <= today {
				aired[e.ID] = true
			}
			if !e.Monitored || e.AirDate == "" || e.AirDate > today {
				eligiblePack = false
			}
		}
	}
	for _, e := range pack.Episodes {
		if aired[e.EpisodeID] && e.Monitored() && wants(profile, e) && len(s.notInFlight(ctx, []domain.Wantable{e})) > 0 {
			if capped, _ := s.regrabCapped(ctx, e); !capped {
				targets = append(targets, e)
			}
		}
	}
	rows, err := s.db.ListDownloadReservations(ctx)
	if err != nil {
		return item, pack, profile, nil, false, err
	}
	for _, dl := range rows {
		if dl.MediaItemID == p.ItemID && dl.CopyID == p.CopyID && dl.Season == p.Season && (persistedDownloadHeld(dl.RunnerControl) || len(dl.WantableIDs) == 0) {
			eligiblePack = false
		}
	}
	return item, pack, profile, targets, eligiblePack, nil
}
func (s *Service) saveSeasonCheckpoint(ctx context.Context, j domain.Job, p seasonCheckpoint) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return s.db.UpdateJobCheckpoint(ctx, j.ID, j.LeaseOwner, string(raw))
}

// feasibleFinish simulates rolling token releases and serial request durations.
// Conservative two-page query bounds reserve one hour before evidence expiry.
func feasibleFinish(now time.Time, b sqlite.IndexerBudget, day, half []time.Time, requests int, duration time.Duration) time.Time {
	at := now
	d := slices.Clone(day)
	h := slices.Clone(half)
	for n := 0; n < requests; n++ {
		for {
			d = slices.DeleteFunc(d, func(t time.Time) bool { return !t.After(at) })
			h = slices.DeleteFunc(h, func(t time.Time) bool { return !t.After(at) })
			if len(d) < b.Search && len(h) < (b.Search+1)/2 {
				break
			}
			next := at.Add(25 * time.Hour)
			if len(d) >= b.Search && len(d) > 0 && d[0].Before(next) {
				next = d[0]
			}
			if len(h) >= (b.Search+1)/2 && len(h) > 0 && h[0].Before(next) {
				next = h[0]
			}
			if b.Search == 0 {
				return next
			}
			at = next
		}
		d = append(d, at.Add(24*time.Hour))
		h = append(h, at.Add(12*time.Hour))
		slices.SortFunc(d, func(a, b time.Time) int { return a.Compare(b) })
		slices.SortFunc(h, func(a, b time.Time) int { return a.Compare(b) })
		at = at.Add(duration)
	}
	return at
}
func (s *Service) handleSeasonSearch(ctx context.Context, j domain.Job) error {
	var p seasonCheckpoint
	if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
		return err
	}
	if p.Version != 1 || p.ItemID == 0 {
		return fmt.Errorf("invalid season checkpoint")
	}
	item, pack, profile, targets, allowPacks, err := s.seasonSnapshot(ctx, p)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	if item.Path == "" && p.CopyID == 0 {
		return ErrNoLibraryFolder
	}
	if p.Fingerprint == "" {
		p.Fingerprint = discoveryFingerprint(pack, profile, targets)
	}
	var activeScope int
	if err = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM acquisition_plans WHERE media_item_id=? AND copy_id=? AND season=? AND state IN ('admitted','dispatching','active','cancel_requested')`, p.ItemID, p.CopyID, p.Season).Scan(&activeScope); err != nil {
		return err
	}
	if activeScope > 0 {
		return &sqlite.BudgetDeferred{At: time.Now().Add(time.Minute), Reason: "waiting for remaining active plan work"}
	}
	revision, err := s.db.AcquisitionRevision(ctx)
	if err != nil {
		return err
	}
	if p.Revision != 0 && revision != p.Revision {
		p.Scopes = nil
		p.Releases = nil
		p.Started = time.Time{}
		p.Stage = ""
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil {
		return err
	}
	if len(enabled) == 0 {
		return ErrNoIndexers
	}
	now := time.Now()
	if p.Started.IsZero() {
		// Completion-first admission: unopened work has no evidence clock. A single
		// open comparison is conservative and never creates partial via rotation.
		var open int
		err = s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=? AND id!=? AND state IN ('queued','leased') AND COALESCE(json_extract(payload,'$.started'),'0001-01-01T00:00:00Z')!='0001-01-01T00:00:00Z'`, SeasonSearchJobKind, j.ID).Scan(&open)
		if err != nil {
			return err
		}
		if open > 0 {
			return &sqlite.BudgetDeferred{At: now.Add(time.Minute), Reason: "waiting for admitted season comparison"}
		}
		for _, cfg := range enabled {
			requests := 7 // capability plus 3 alternatives × 2 pages
			for _, e := range targets {
				if !e.OnDisk() || p.Deep {
					requests += 6
				}
			}
			b, d, h, e := s.db.RequestCapacity(ctx, cfg.ID, now)
			if e != nil {
				return e
			}
			deadline := now.Add(23 * time.Hour)
			empty := feasibleFinish(now, b, nil, nil, requests, s.searchTimeout)
			if empty.After(deadline) {
				p.IntrinsicPartial = true
				continue
			}
			if finish := feasibleFinish(now, b, d, h, requests, s.searchTimeout); finish.After(deadline) {
				return &sqlite.BudgetDeferred{At: now.Add(time.Hour), Reason: "waiting for capacity to finish a complete season comparison"}
			}
		}
		p.Started = now
		p.Deadline = now.Add(24 * time.Hour)
		p.Revision = revision
		p.Stage = "season"
		if err = s.saveSeasonCheckpoint(ctx, j, p); err != nil {
			return err
		}
	}
	if now.After(p.Deadline.Add(-time.Hour)) {
		// Scheduling expiration refreshes; it grants no incomplete dispatch.
		p.Started = time.Time{}
		p.Scopes = nil
		p.Releases = nil
		p.Stage = ""
		p.SchedulingDeferred = true
		if err = s.saveSeasonCheckpoint(ctx, j, p); err != nil {
			return err
		}
		return &sqlite.BudgetDeferred{At: now.Add(time.Minute), Reason: "refreshing interrupted discovery evidence"}
	}
	if len(p.Scopes) == 0 {
		for _, cfg := range enabled {
			p.Scopes = append(p.Scopes, discoveryScope{IndexerID: cfg.ID})
		}
		if err = s.saveSeasonCheckpoint(ctx, j, p); err != nil {
			return err
		}
	}
	for {
		for i := range p.Scopes {
			sc := &p.Scopes[i]
			if sc.Complete || sc.Degraded {
				continue
			}
			if time.Now().Before(sc.RetryAt) {
				return &sqlite.BudgetDeferred{At: sc.RetryAt, Reason: "provider search deferred"}
			}
			var cfg ports.IndexerConfig
			for _, c := range enabled {
				if c.ID == sc.IndexerID {
					cfg = c
				}
			}
			indexer := s.budgetIndexer(cfg, "search", true)
			if len(sc.Queries) == 0 {
				var scopeWantable domain.Wantable = pack
				if sc.EpisodeID != 0 {
					for _, ep := range targets {
						if ep.EpisodeID == sc.EpisodeID {
							scopeWantable = ep
						}
					}
				}
				callCtx, cancel := context.WithTimeout(ctx, s.searchTimeout)
				queries, reasons, e := queriesForIndexer(callCtx, indexer, scopeWantable, false)
				cancel()
				if e != nil {
					s.rememberIndexerDelay(ctx, cfg.ID, e)
					var deferred interface{ DeferredUntil() time.Time }
					var remote *ports.RemoteError
					if errors.As(e, &deferred) {
						if er := s.saveSeasonCheckpoint(ctx, j, p); er != nil {
							return er
						}
						return e
					}
					sc.Failures++
					if sc.Failures >= 3 || terminalSearchError(e) {
						sc.Degraded = true
						sc.Complete = true
						sc.Reasons = append(sc.Reasons, searchFailureReason(e))
					} else {
						sc.RetryAt = time.Now().Add(time.Minute)
						if errors.As(e, &remote) && !remote.RetryAt.IsZero() {
							sc.RetryAt = remote.RetryAt
						}
					}
					if er := s.saveSeasonCheckpoint(ctx, j, p); er != nil {
						return er
					}
					if !sc.Complete {
						return &sqlite.BudgetDeferred{At: sc.RetryAt, Reason: "retrying capability scope"}
					}
					continue
				}
				sc.Queries = queries
				sc.Reasons = append(sc.Reasons, reasons...)
				sc.RetryAt = time.Time{}
				if len(queries) == 0 {
					sc.Complete = true
					sc.Degraded = true
				}
				if e = s.saveSeasonCheckpoint(ctx, j, p); e != nil {
					return e
				}
				if sc.Complete {
					continue
				}
			}
			for sc.Query < len(sc.Queries) {
				q := sc.Queries[sc.Query]
				callCtx, cancel := context.WithTimeout(ctx, s.searchTimeout)
				var rows []ports.Release
				complete := true
				next := 0
				if pager, ok := indexer.(ports.PagedIndexer); ok {
					page, e := pager.SearchPage(callCtx, q, sc.Offset)
					err = e
					rows = page.Releases
					complete = page.Complete
					next = page.NextOffset
					if !complete && next <= sc.Offset {
						sc.Reasons = append(sc.Reasons, "malformed pagination continuation")
						complete = true
					}
				} else {
					rows, err = indexer.Search(callCtx, q)
				}
				cancel()
				if err != nil {
					s.rememberIndexerDelay(ctx, cfg.ID, err)
					var deferred interface{ DeferredUntil() time.Time }
					var remote *ports.RemoteError
					if errors.As(err, &deferred) {
						if e := s.saveSeasonCheckpoint(ctx, j, p); e != nil {
							return e
						}
						if p.IntrinsicPartial && p.Stage == "episodes" {
							if e := s.admitSeasonCandidates(ctx, p, item, pack, profile, targets, false); e != nil {
								return e
							}
						}
						return err
					}
					if errors.As(err, &remote) && !remote.RetryAt.IsZero() {
						sc.RetryAt = remote.RetryAt
						if e := s.saveSeasonCheckpoint(ctx, j, p); e != nil {
							return e
						}
						return &sqlite.BudgetDeferred{At: remote.RetryAt, Reason: "provider RetryAt"}
					}
					sc.Failures++
					if sc.Failures >= 3 || terminalSearchError(err) {
						sc.Degraded = true
						sc.Complete = true
						sc.Reasons = append(sc.Reasons, searchFailureReason(err))
					} else {
						sc.RetryAt = time.Now().Add(time.Minute)
					}
					if e := s.saveSeasonCheckpoint(ctx, j, p); e != nil {
						return e
					}
					if !sc.Complete {
						return &sqlite.BudgetDeferred{At: sc.RetryAt, Reason: "retrying incomplete provider scope"}
					}
					break
				}
				if sc.Failures > 0 {
					sc.Reasons = slices.DeleteFunc(sc.Reasons, func(reason string) bool {
						return strings.HasPrefix(reason, "search failed") || strings.HasPrefix(reason, "timeout") || strings.HasPrefix(reason, "remote") || reason == "indexer error" || reason == "network" || reason == "server_error"
					})
				}
				previousReleases := slices.Clone(p.Releases)
				p.Releases = deduplicateReleases(append(p.Releases, rows...))
				raw, _ := json.Marshal(p)
				if len(raw) > 2<<20 {
					// The wire query ran, but its evidence cannot fit. Retain
					// earlier complete scopes, explicitly refuse this scope's
					// absence/optimality claims, and never repeat the same page.
					p.Releases = previousReleases
					sc.Reasons = append(sc.Reasons, "discovery checkpoint exceeded 2 MiB; query evidence incomplete")
					sc.Query = len(sc.Queries)
					sc.Complete = true
					if e := s.saveSeasonCheckpoint(ctx, j, p); e != nil {
						return e
					}
					break
				}
				sc.Pages++
				stop := false
				if sc.EpisodeID != 0 {
					for _, ep := range targets {
						if ep.EpisodeID == sc.EpisodeID {
							stop = s.hasTargetSingle(ctx, p.Releases, ep, profile)
						}
					}
				}
				if !complete && !stop && sc.Pages < 2 {
					sc.Offset = next
				} else {
					if !complete && !stop {
						sc.Reasons = append(sc.Reasons, "advertised pages exceed bounded query")
					}
					sc.Query++
					sc.Offset = 0
					sc.Pages = 0
				}
				if stop {
					sc.Query = len(sc.Queries)
				}
				if e := s.saveSeasonCheckpoint(ctx, j, p); e != nil {
					return e
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			sc.Complete = true
		}
		if p.Stage != "season" {
			break
		}
		p.Stage = "episodes"
		for _, ep := range targets {
			if (ep.OnDisk() && !p.Deep) || s.hasTargetSingle(ctx, p.Releases, ep, profile) {
				continue
			}
			for _, cfg := range enabled {
				excluded := false
				for _, sc := range p.Scopes {
					if sc.IndexerID == cfg.ID && sc.Degraded {
						excluded = true
					}
				}
				if excluded {
					continue
				}
				p.Scopes = append(p.Scopes, discoveryScope{IndexerID: cfg.ID, EpisodeID: ep.EpisodeID})
			}
		}
		if err = s.saveSeasonCheckpoint(ctx, j, p); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err = s.admitSeasonCandidates(ctx, p, item, pack, profile, targets, allowPacks)
	if err == nil {
		incomplete := p.IntrinsicPartial
		for _, sc := range p.Scopes {
			incomplete = incomplete || sc.Degraded || len(sc.Reasons) > 0
		}
		if incomplete {
			p.Started = time.Time{}
			p.Scopes = nil
			p.Stage = ""
			p.Revision = 0
			p.Releases = nil
			if e := s.saveSeasonCheckpoint(ctx, j, p); e != nil {
				return e
			}
			return &sqlite.BudgetDeferred{At: time.Now().Add(24 * time.Hour), Reason: "new discovery epoch after genuine incomplete comparison"}
		}
		p.CooldownUntil = time.Now().Add(7 * 24 * time.Hour)
		if e := s.saveSeasonCheckpoint(ctx, j, p); e != nil {
			return e
		}
	}
	return err
}
func (s *Service) hasTargetSingle(ctx context.Context, rs []ports.Release, ep domain.EpisodeWantable, profile quality.Profile) bool {
	index, err := s.allIdentity(ctx, time.Now())
	if err != nil {
		return false
	}
	for _, r := range rs {
		parsed := parser.Parse(r.Title)
		if parsed.SeasonPack || len(parsed.Episodes) != 1 || !releaseMatch(r, parsed, ep, index).Matched || s.isBlocklisted(ctx, r.Title, r.Indexer) {
			continue
		}
		if !decision.Decide(releaseOf(parsed), ep, profile).Accepted || !profile.Met(parsed.Quality, true) {
			continue
		}
		if _, bad := sizeImplausible(parsed.Quality, r, s.db.ItemRuntime(ctx, ep.Item)); !bad {
			return true
		}
	}
	return false
}

// Decision facts are public; transport remains on private download intents.
type seasonDecision struct {
	Version            int                                      `json:"version"`
	OriginalDownloadID int64                                    `json:"originalDownloadId,omitempty"`
	Trigger            string                                   `json:"trigger"`
	Target             string                                   `json:"target"`
	Partial            bool                                     `json:"partial"`
	Scopes             []discoveryScope                         `json:"scopes"`
	Plan               acquisitionplan.Plan                     `json:"plan"`
	Candidates         []acquisitionplan.Candidate              `json:"candidates"`
	Titles             map[acquisitionplan.CandidateKey]string  `json:"titles"`
	ImportAllowlists   map[acquisitionplan.CandidateKey][]int64 `json:"importAllowlists"`
	Reason             string                                   `json:"reason"`
}

func (s *Service) admitSeasonCandidates(ctx context.Context, p seasonCheckpoint, item domain.MediaItem, pack domain.SeasonWantable, profile quality.Profile, targets []domain.EpisodeWantable, allowPacks bool) error {
	index, err := s.allIdentity(ctx, time.Now())
	if err != nil {
		return err
	}
	formats, err := s.db.ListCustomFormats(ctx)
	if err != nil {
		return err
	}
	runtime := s.db.ItemRuntime(ctx, item.ID)
	in := acquisitionplan.Input{AllowPacks: allowPacks}
	byEp := map[acquisitionplan.EpisodeKey]domain.EpisodeWantable{}
	for _, e := range targets {
		k := acquisitionplan.EpisodeKey(e.EpisodeID)
		in.Episodes = append(in.Episodes, k)
		byEp[k] = e
	}
	partial := p.IntrinsicPartial
	for _, sc := range p.Scopes {
		partial = partial || !sc.Complete
	}
	packIncomplete := p.IntrinsicPartial
	for _, sc := range p.Scopes {
		if len(sc.Reasons) > 0 || sc.Degraded {
			partial = true
		}
		if !sc.Degraded && (len(sc.Reasons) > 0 || !sc.Complete) {
			packIncomplete = true
		}
	}
	releases := map[acquisitionplan.CandidateKey]ports.Release{}
	evidence := map[acquisitionplan.CandidateKey]domain.MatchEvidence{}
	titles := map[acquisitionplan.CandidateKey]string{}
	for _, r := range p.Releases {
		excluded := false
		for _, sc := range p.Scopes {
			if sc.IndexerID == r.IndexerID && sc.Degraded {
				excluded = true
			}
		}
		if excluded {
			continue
		}
		parsed := parser.Parse(r.Title)
		isPack := parsed.SeasonPack
		if s.isBlocklisted(ctx, r.Title, r.Indexer) || isPack && (!allowPacks || packIncomplete) || !isPack && len(parsed.Episodes) != 1 {
			continue
		}
		var w domain.Wantable = pack
		if !isPack {
			for _, e := range targets {
				if e.Season == parsed.Season && e.Episode == parsed.Episodes[0] {
					w = e
					break
				}
			}
			if _, ok := w.(domain.EpisodeWantable); !ok {
				continue
			}
		}
		match := releaseMatch(r, parsed, w, index)
		if !match.Matched {
			continue
		}
		if _, bad := sizeImplausible(parsed.Quality, r, runtime); bad {
			continue
		}
		hash := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s", r.IndexerID, r.GUID, r.Title, r.DownloadURL)))
		key := acquisitionplan.CandidateKey(fmt.Sprintf("%x", hash[:16]))
		class := quality.Rank(parsed.Quality)
		if profile.Met(parsed.Quality, true) {
			class = acquisitionplan.TargetMet
		}
		c := acquisitionplan.Candidate{Key: key, Pack: isPack, Pref: acquisitionplan.Preference{Class: class, QualityRank: quality.Rank(parsed.Quality), FormatScore: format.Score(r.Title, formats)}, SizeBytes: r.Size, SizeKnown: r.Size > 0, Protocol: r.Protocol, Seeders: r.Seeders, SeedersKnown: r.SeedersKnown}
		if isPack {
			for _, e := range pack.Episodes {
				c.Payload = append(c.Payload, acquisitionplan.EpisodeKey(e.EpisodeID))
			}
		} else {
			c.Payload = []acquisitionplan.EpisodeKey{acquisitionplan.EpisodeKey(w.(domain.EpisodeWantable).EpisodeID)}
		}
		for _, e := range targets {
			if !isPack && e.ID() != w.ID() {
				continue
			}
			if !decision.Decide(releaseOf(parsed), e, profile).Accepted {
				continue
			}
			completed := true
			if partial && !isPack {
				for _, cfg := range enabledForScopes(p.Scopes) {
					found := false
					excluded := false
					for _, sc := range p.Scopes {
						if sc.IndexerID != cfg {
							continue
						}
						if sc.Degraded {
							excluded = true
						}
						if sc.EpisodeID == e.EpisodeID && sc.Complete && len(sc.Reasons) == 0 {
							found = true
						}
						if sc.EpisodeID == 0 && sc.Complete && len(sc.Reasons) == 0 {
							var same []ports.Release
							for _, known := range p.Releases {
								if known.IndexerID == cfg {
									same = append(same, known)
								}
							}
							if s.hasTargetSingle(ctx, same, e, profile) {
								found = true
							}
						}
					}
					if !found && !excluded {
						completed = false
					}
				}
			}
			if completed {
				c.Eligible = append(c.Eligible, acquisitionplan.EpisodeKey(e.EpisodeID))
			}
		}
		if len(c.Eligible) == 0 {
			continue
		}
		in.Candidates = append(in.Candidates, c)
		releases[key] = r
		evidence[key] = matchEvidence(parsed, w, r, match)
		titles[key] = r.Title
	}
	plan, err := acquisitionplan.Build(in)
	if err != nil {
		return err
	}
	if len(plan.Releases) == 0 {
		return nil
	}
	decisionFacts := seasonDecision{Version: 1, OriginalDownloadID: p.OriginalDownloadID, Trigger: p.Trigger, Target: profile.Target.Display(), Partial: partial, Scopes: p.Scopes, Plan: plan, Candidates: in.Candidates, Titles: titles, ImportAllowlists: map[acquisitionplan.CandidateKey][]int64{}, Reason: "Compared attainable episode outcomes up to your target, then torrent health and full transfer cost; protected existing files are kept."}
	decisionFacts.Reason = selectionReason(plan, in.Candidates, partial, packIncomplete)

	for e, k := range plan.Providers {
		decisionFacts.ImportAllowlists[k] = append(decisionFacts.ImportAllowlists[k], int64(e))
	}
	for k, v := range decisionFacts.ImportAllowlists {
		slices.Sort(v)
		decisionFacts.ImportAllowlists[k] = v
	}
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return err
	}
	priority, err := s.downloadPriority(ctx, item, p.CopyID)
	if err != nil {
		return err
	}
	var intents []sqlite.Download
	for _, key := range plan.Releases {
		r := releases[key]
		var cfg *ports.ClientConfig
		for i := range clients {
			if clients[i].Enabled && protocolOfClient(clients[i].Type) == r.Protocol {
				cfg = &clients[i]
				break
			}
		}
		if cfg == nil {
			return ErrNoClient
		}
		exec, _ := json.Marshal(plannedExecution{URL: r.DownloadURL, Category: cfg.Category, Priority: priority, ClientFingerprint: clientFingerprint(*cfg)})
		dl := sqlite.Download{MediaItemID: item.ID, CopyID: p.CopyID, Season: p.Season, ReleaseTitle: r.Title, Indexer: r.Indexer, Protocol: r.Protocol, Quality: parser.Parse(r.Title).Quality, Size: r.Size, ClientID: cfg.ID, CandidateKey: string(key), ExecutionPayload: string(exec), ReservedEpisodes: decisionFacts.ImportAllowlists[key], MatchEvidence: evidence[key]}
		for _, id := range dl.ReservedEpisodes {
			dl.WantableIDs = append(dl.WantableIDs, string(byEp[acquisitionplan.EpisodeKey(id)].ID()))
		}
		intents = append(intents, dl)
	}
	dest := item.Path
	if p.CopyID != 0 {
		cp, e := s.db.GetMediaCopy(ctx, item.ID, p.CopyID)
		if e != nil {
			return e
		}
		if cp.Path != "" {
			dest = cp.Path
		}
	}
	if dest == "" {
		return ErrNoLibraryFolder
	}
	if plan.Cost.KnownBytes > (1<<62)-1 {
		return fmt.Errorf("plan staging size overflow")
	}
	if err = s.checkStorageIdentity(ctx, dest); err != nil {
		return err
	}
	if err = recoveryCapacity(dest, 2*plan.Cost.KnownBytes); err != nil {
		return err
	}
	// The import lock serializes live file reread and durable admission with
	// publication/fallback. Conditional revision catches non-import edits.
	s.importTargetMu.Lock()
	_, _, _, fresh, _, err := s.seasonSnapshot(ctx, p)
	if err == nil && len(fresh) != len(targets) {
		err = fmt.Errorf("season targets changed")
	}
	var id int64
	if err == nil {
		raw, _ := json.Marshal(decisionFacts)
		id, err = s.db.AdmitAcquisitionPlan(ctx, sqlite.AcquisitionPlan{MediaItemID: item.ID, CopyID: p.CopyID, Season: p.Season, SnapshotFingerprint: p.Fingerprint, Decision: raw}, p.Revision, intents)
	}
	s.importTargetMu.Unlock()
	if err != nil {
		return err
	}
	return s.dispatchPlan(ctx, id)
}

func missingFirst(ws []domain.Wantable) []domain.Wantable {
	out := make([]domain.Wantable, 0, len(ws))
	for _, w := range ws {
		if !w.OnDisk() {
			out = append(out, w)
		}
	}
	for _, w := range ws {
		if w.OnDisk() {
			out = append(out, w)
		}
	}
	return out
}
func (s *Service) enqueueItemSeasons(ctx context.Context, item domain.MediaItem) (AutoSearchOutcome, error) {
	out := AutoSearchOutcome{Targets: []AutoSearchTarget{}}
	copies := []int64{0}
	for _, cp := range item.Copies {
		if cp.Monitored {
			copies = append(copies, cp.ID)
		}
	}
	for _, copyID := range copies {
		for _, season := range item.Seasons {
			if !season.Monitored {
				continue
			}
			p := seasonCheckpoint{ItemID: item.ID, CopyID: copyID, Season: season.Number}
			_, pack, _, targets, _, err := s.seasonSnapshot(ctx, p)
			if err != nil {
				return out, err
			}
			if len(targets) == 0 {
				continue
			}
			missing := false
			for _, ep := range targets {
				missing = missing || !ep.OnDisk()
			}
			if err = s.enqueueSeason(ctx, item.ID, copyID, season.Number, "item_search", missing); err != nil {
				return out, err
			}
			out.Targets = append(out.Targets, AutoSearchTarget{WantableID: string(pack.ID()), Label: describeTarget(pack), Skipped: "queued"})
		}
	}
	return out, nil
}

func discoveryFingerprint(pack domain.SeasonWantable, profile quality.Profile, targets []domain.EpisodeWantable) string {
	raw, _ := json.Marshal(struct {
		Pack    domain.SeasonWantable
		Profile quality.Profile
		Targets []domain.EpisodeWantable
	}{pack, profile, targets})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum)
}
func (s *Service) enqueueRSSSeason(ctx context.Context, ep domain.EpisodeWantable, r ports.Release, profile quality.Profile) error {
	var payload string
	err := s.db.R.QueryRowContext(ctx, `SELECT payload FROM jobs WHERE kind=? AND dedupe_key=? ORDER BY id DESC LIMIT 1`, SeasonSearchJobKind, seasonJobKey(ep.Item, ep.Copy, ep.Season)).Scan(&payload)
	if err == nil {
		var old seasonCheckpoint
		if json.Unmarshal([]byte(payload), &old) == nil {
			wanted := quality.Rank(parser.Parse(r.Title).Quality)
			if profile.Met(parser.Parse(r.Title).Quality, true) {
				wanted = acquisitionplan.TargetMet
			}
			best := -1
			for _, known := range old.Releases {
				p := parser.Parse(known.Title)
				if !p.SeasonPack && (p.Season != ep.Season || !slices.Contains(p.Episodes, ep.Episode)) {
					continue
				}
				if !decision.Decide(releaseOf(p), ep, profile).Accepted {
					continue
				}
				class := quality.Rank(p.Quality)
				if profile.Met(p.Quality, true) {
					class = acquisitionplan.TargetMet
				}
				best = max(best, class)
			}
			if wanted <= best {
				return nil
			}
		}
	}
	if err = s.enqueueSeason(ctx, ep.Item, ep.Copy, ep.Season, "rss_improvement", !ep.OnDisk()); err != nil {
		return err
	}
	// Merge fresh feed evidence only into queued checkpoints; lease owners alone
	// update running payloads. This response is supply evidence, never absence.
	rows, e := s.db.R.QueryContext(ctx, `SELECT id,payload FROM jobs WHERE kind=? AND dedupe_key=? AND state='queued'`, SeasonSearchJobKind, seasonJobKey(ep.Item, ep.Copy, ep.Season))
	if e != nil {
		return e
	}
	var id int64
	var raw string
	for rows.Next() {
		if e = rows.Scan(&id, &raw); e != nil {
			break
		}
	}
	_ = rows.Close()
	if e != nil {
		return e
	}
	if id == 0 {
		return nil
	}
	var checkpoint seasonCheckpoint
	if e = json.Unmarshal([]byte(raw), &checkpoint); e != nil {
		return e
	}
	checkpoint.Releases = deduplicateReleases(append(checkpoint.Releases, r))
	updated, e := json.Marshal(checkpoint)
	if e != nil {
		return e
	}
	if len(updated) > 2<<20 {
		return nil
	}
	_, e = s.db.W.ExecContext(ctx, `UPDATE jobs SET payload=?,updated_at=? WHERE id=? AND state='queued' AND payload=?`, string(updated), time.Now().UnixMilli(), id, raw)
	return e
}

func enabledForScopes(scopes []discoveryScope) []int64 {
	var ids []int64
	for _, sc := range scopes {
		if !slices.Contains(ids, sc.IndexerID) {
			ids = append(ids, sc.IndexerID)
		}
	}
	return ids
}

func selectionReason(plan acquisitionplan.Plan, candidates []acquisitionplan.Candidate, partial, packIncomplete bool) string {
	packs, singles, gaps := 0, 0, 0
	for _, key := range plan.Releases {
		for _, c := range candidates {
			if c.Key == key {
				if c.Pack {
					packs++
				} else {
					singles++
				}
			}
		}
	}
	for ep, key := range plan.Providers {
		for _, c := range candidates {
			if c.Key != key || !c.Pack {
				continue
			}
			equivalent := false
			for _, single := range candidates {
				if !single.Pack && slices.Contains(single.Eligible, ep) && single.Pref.Class >= plan.Outcomes[ep] {
					equivalent = true
				}
			}
			if !equivalent {
				gaps++
			}
		}
	}
	reason := fmt.Sprintf("Selected %d season pack(s) and %d single(s) to attain the best eligible outcome for each episode, up to your target.", packs, singles)
	if gaps > 0 {
		reason += fmt.Sprintf(" The pack supplies %d episode(s) without an equivalent acceptable single in this pool.", gaps)
	}
	if partial && packIncomplete {
		reason += " Completed episode searches permit singles; incomplete season comparison remains deferred."
	} else if partial {
		reason += " Unavailable provider scopes were excluded; healthy provider discovery completed."
	}
	reason += " Equivalent outcomes were compared by known-zero torrent risk, unknown sizes, full advertised bytes, unknown health, redundant episodes, and transfer count. Source and format only break remaining ties. Existing protected files are kept."
	return reason
}
