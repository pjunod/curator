// Package recommendation orchestrates bounded, read-only series discovery.
package recommendation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	rules "github.com/pjunod/monarr/internal/domain/recommendation"
	"github.com/pjunod/monarr/internal/ports"
)

var ErrInvalidRequest = errors.New("invalid_recommendation_request")
var ErrBusy = errors.New("recommendations_busy")
var ErrStateChanged = errors.New("recommendation_state_changed")
var ErrUnavailable = errors.New("provider_unavailable")

type Result struct {
	Item            ports.SearchResult
	Facts           rules.Facts
	Evidence        rules.Evidence
	Ownership       ports.Ownership
	Score, Metadata float64
}
type Response struct {
	SeedCompared                                                  bool
	State, Ranking, ModelState, Coverage                          string
	Applied                                                       rules.Applied
	Warnings                                                      []rules.Warning
	Results                                                       []Result
	RetrievedCount, CheckedCount, EligibleCount, HiddenOwnedCount int
}
type entry struct {
	value             Response
	expires, timeUsed time.Time
	bytes             int
}
type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   Response
	err     error
}
type Service struct {
	source    ports.RecommendationSource
	ownership ports.RecommendationOwnership
	encoder   ports.SentenceEncoder
	mu        sync.Mutex
	cache     map[string]entry
	bytes     int
	active    *flight
	activeKey string
	readers   int
}

func New(source ports.RecommendationSource, ownership ports.RecommendationOwnership, encoder ports.SentenceEncoder) *Service {
	return &Service{source: source, ownership: ownership, encoder: encoder, cache: map[string]entry{}}
}
func (s *Service) model() (string, uint64) {
	if s.encoder == nil {
		return "disabled", 0
	}
	return s.encoder.State()
}
func (s *Service) Status(ctx context.Context) (string, bool) {
	state, _ := s.model()
	_, _, err := s.source.Snapshot(ctx)
	return state, err == nil
}

func (s *Service) Search(ctx context.Context, r rules.Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	applied, warnings, err := rules.Normalize(r, time.Now())
	if err != nil {
		return Response{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	state, generation := s.model()
	if len(warnings) > 0 {
		return Response{State: "needs_refinement", Applied: applied, Warnings: warnings, Ranking: "metadata_only", ModelState: state, Coverage: "bounded", Results: []Result{}}, nil
	}
	if applied.Seed != nil {
		id, e := strconv.ParseInt(applied.Seed.Value, 10, 64)
		if e != nil || id <= 0 || applied.Seed.Provider != "tmdb" && applied.Seed.Provider != "tvdb" {
			return Response{}, fmt.Errorf("%w: seed must be a positive TMDB or TVDB series ID", ErrInvalidRequest)
		}
	}
	_, provider, err := s.source.Snapshot(ctx)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	data, _ := json.Marshal(applied)
	key := fmt.Sprintf("%x:%s:%d:%s:%s", sha256.Sum256(data), provider, generation, state, rules.RankingVersion)
	s.mu.Lock()
	if cached, ok := s.cache[key]; ok && time.Now().Before(cached.expires) {
		if s.readers >= 2 {
			s.mu.Unlock()
			return Response{}, ErrBusy
		}
		s.readers++
		cached.timeUsed = time.Now()
		s.cache[key] = cached
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.readers--; s.mu.Unlock() }()
		cached.value.Applied = applied
		value, err := s.publish(ctx, cached.value, provider, generation)
		if !time.Now().Before(cached.expires) {
			return Response{}, ErrStateChanged
		}
		return value, err
	}
	f := s.active
	if f != nil {
		if s.activeKey != key || f.waiters >= 4 {
			s.mu.Unlock()
			return Response{}, ErrBusy
		}
		f.waiters++
	} else {
		work, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		f = &flight{done: make(chan struct{}), cancel: cancel, waiters: 1}
		s.active = f
		s.activeKey = key
		go s.run(work, f, key, applied, provider, generation)
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
		}
		s.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-f.done:
		if f.err != nil {
			return Response{}, f.err
		}
		return s.publish(ctx, f.value, provider, generation)
	}
}

func (s *Service) run(ctx context.Context, f *flight, key string, a rules.Applied, provider string, generation uint64) {
	value, err := s.retrieve(ctx, a, provider, generation)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer f.cancel()
	f.value, f.err = value, err
	if err == nil && value.State == "ready" && value.ModelState != "failed" && ctx.Err() == nil && f.waiters > 0 {
		// Cache only result facts and normalized filters; prose belongs to the
		// current request and is restored on hits, never retained in entries.
		value.Applied.Query = ""
		value.Applied.RankingText = ""
		data, _ := json.Marshal(value)
		size := len(data)
		if old, ok := s.cache[key]; ok {
			s.bytes -= old.bytes
			delete(s.cache, key)
		}
		for len(s.cache) >= 128 || s.bytes+size > 64<<20 {
			oldest := ""
			var age time.Time
			for k, e := range s.cache {
				if oldest == "" || e.timeUsed.Before(age) {
					oldest, age = k, e.timeUsed
				}
			}
			if oldest == "" {
				break
			}
			s.bytes -= s.cache[oldest].bytes
			delete(s.cache, oldest)
		}
		if size <= 64<<20 {
			s.cache[key] = entry{value: value, expires: time.Now().Add(5 * time.Minute), timeUsed: time.Now(), bytes: size}
			s.bytes += size
		}
	}
	if s.active == f {
		s.active = nil
		s.activeKey = ""
	}
	close(f.done)
}

type candidate struct {
	item  ports.SearchResult
	ranks map[string]int
}

func merge(pages []ports.CandidatePage, names []string, pageNumbers []int) []candidate {
	var all []candidate
	for i, p := range pages {
		for j, item := range p.Items {
			all = append(all, candidate{item: item, ranks: map[string]int{names[i]: (pageNumbers[i]-1)*20 + j + 1}})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		rank := func(c candidate) int {
			for _, r := range c.ranks {
				return r
			}
			return 0
		}
		return rank(all[i]) < rank(all[j])
	})
	return unique(all, 60)
}
func unique(items []candidate, limit int) []candidate {
	out := []candidate{}
	seen := map[int64]int{}
	for _, c := range items {
		if i, ok := seen[c.item.TMDBID]; ok {
			for k, r := range c.ranks {
				if old, exists := out[i].ranks[k]; !exists || r < old {
					out[i].ranks[k] = r
				}
			}
			continue
		}
		if len(out) < limit {
			seen[c.item.TMDBID] = len(out)
			out = append(out, c)
		}
	}
	return out
}
func rr(c candidate) float64 {
	sum := 0.
	for _, r := range c.ranks {
		sum += 1 / float64(60+r)
	}
	return sum
}
func combined(theme, seed []candidate, limit int) []candidate {
	out := []candidate{}
	seen := map[int64]bool{}
	add := func(c candidate) {
		if !seen[c.item.TMDBID] && len(out) < limit {
			seen[c.item.TMDBID] = true
			out = append(out, c)
		}
	}
	for _, path := range [][]candidate{theme, seed} {
		n := 0
		for _, c := range path {
			if !seen[c.item.TMDBID] {
				add(c)
				n++
				if n == limit/2 {
					break
				}
			}
		}
	}
	for i := 0; i < len(theme) || i < len(seed); i++ {
		if i < len(theme) {
			add(theme[i])
		}
		if i < len(seed) {
			add(seed[i])
		}
	}
	for i := range out {
		for _, path := range [][]candidate{theme, seed} {
			for _, c := range path {
				if c.item.TMDBID == out[i].item.TMDBID {
					for k, r := range c.ranks {
						out[i].ranks[k] = r
					}
				}
			}
		}
	}
	return out
}

func (s *Service) retrieve(ctx context.Context, a rules.Applied, provider string, generation uint64) (Response, error) {
	state, _ := s.model()
	out := Response{State: "ready", Applied: a, Ranking: "metadata_only", ModelState: state, Coverage: "bounded", Results: []Result{}}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pctx, snapshot, err := s.source.Snapshot(pctx)
	if err != nil || snapshot != provider {
		return out, ErrStateChanged
	}
	partial := func() { out.Coverage = "partial" }
	var seed rules.Facts
	if a.Seed != nil {
		seed, err = s.source.ResolveSeed(pctx, *a.Seed)
		if err != nil {
			return out, err
		}
	}
	var theme, related []candidate
	if a.Filters.Theme != nil {
		t := rules.FindTheme(*a.Filters.Theme)
		resolved := map[string]int64{}
		failed := 0
		for _, alias := range t.Aliases {
			ks, e := s.source.ResolveKeyword(pctx, alias)
			if e != nil {
				failed++
				partial()
				continue
			}
			if len(ks) == 1 {
				resolved[alias] = ks[0].ID
			}
		}
		var pages []ports.CandidatePage
		var names []string
		var numbers []int
		for _, lane := range t.Lanes {
			if id := resolved[lane]; id > 0 {
				p, e := s.source.Candidates(pctx, ports.CandidateRequest{Path: "theme", KeywordID: id, Page: 1, Filters: a.Filters})
				if e != nil {
					partial()
					continue
				}
				pages = append(pages, p)
				names = append(names, "theme:"+lane)
				numbers = append(numbers, 1)
			}
		}
		if len(pages) == 0 {
			if failed > 0 || len(resolved) > 0 {
				return out, ErrUnavailable
			}
			out.State = "needs_refinement"
			out.Warnings = append(out.Warnings, rules.Warning{Code: "theme_unavailable", Message: "No exact provider keyword is available for this theme."})
			return out, nil
		}
		theme = merge(pages, names, numbers)
	}
	if a.Seed != nil {
		var pages []ports.CandidatePage
		recommendationPages := 0
		recommendationFirstKnown := false
		names := []string{}
		numbers := []int{}
		for i, path := range []string{"recommendations", "similar", "recommendations"} {
			page := 1
			if i == 2 {
				page = 2
				if recommendationFirstKnown && recommendationPages < 2 {
					continue
				}
			}
			p, e := s.source.Candidates(pctx, ports.CandidateRequest{Path: path, SeedID: seed.IDs.TMDB, Page: page})
			if e != nil {
				partial()
				continue
			}
			pages = append(pages, p)
			if path == "recommendations" && page == 1 {
				recommendationPages = p.TotalPages
				recommendationFirstKnown = true
			}
			names = append(names, path)
			numbers = append(numbers, page)
		}
		if len(pages) == 0 && len(theme) == 0 {
			return out, ErrUnavailable
		}
		related = merge(pages, names, numbers)
		if a.Filters.Theme == nil {
			var topic []candidate
			topicPages := 0
			for _, alias := range []string{"boys' love (bl)", "space travel"} {
				if id := seed.KeywordIDs[alias]; id > 0 {
					p, e := s.source.Candidates(pctx, ports.CandidateRequest{Path: "theme", KeywordID: id, Page: 1, Filters: a.Filters})
					if e != nil {
						partial()
						continue
					}
					topic = append(topic, merge([]ports.CandidatePage{p}, []string{"topic:" + alias}, []int{1})...)
					topicPages++
				}
			}
			if len(topic) > 0 {
				quota := 40
				if topicPages > 1 {
					quota = 20
				}
				mixed := append(slices.Clone(related[:min(quota, len(related))]), topic...)
				mixed = append(mixed, related[min(quota, len(related)):]...)
				related = unique(mixed, 60)
			}
		}
	}
	removeSeed := func(items []candidate) []candidate {
		return slices.DeleteFunc(items, func(c candidate) bool { return seed.IDs.TMDB > 0 && c.item.TMDBID == seed.IDs.TMDB })
	}
	theme, related = removeSeed(theme), removeSeed(related)
	pool := theme
	if a.Filters.Theme == nil {
		pool = related
	} else if a.Seed != nil {
		pool = combined(theme, related, 60)
	}
	out.RetrievedCount = len(pool)
	sort.SliceStable(theme, func(i, j int) bool {
		return rules.SummaryScore(theme[i].item.Overview, *a.Filters.Theme) > rules.SummaryScore(theme[j].item.Overview, *a.Filters.Theme)
	})
	selected := pool
	ceiling := 30
	if a.Seed != nil && a.Filters.Theme != nil {
		ceiling = 28
		retained := map[int64]bool{}
		for _, c := range pool {
			retained[c.item.TMDBID] = true
		}
		filter := func(path []candidate) []candidate {
			return slices.DeleteFunc(slices.Clone(path), func(c candidate) bool { return !retained[c.item.TMDBID] })
		}
		selected = combined(filter(theme), filter(related), ceiling)
	} else if a.Filters.Theme != nil {
		selected = theme
	}
	if a.Seed != nil && a.Seed.Provider == "tvdb" {
		ceiling--
	}
	rankSpent := time.Duration(0)
	encode := func(texts []string) ([][]float32, error) {
		if s.encoder == nil || state != "ready" {
			return nil, errors.New("encoder unavailable")
		}
		remaining := 4*time.Second - rankSpent
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		ec, done := context.WithTimeout(ctx, remaining)
		start := time.Now()
		vectors, e := s.encoder.Encode(ec, texts)
		rankSpent += time.Since(start)
		done()
		return vectors, e
	}
	if a.Filters.Theme == nil && rules.UsableSeed(seed) && state == "ready" && len(pool) > 0 {
		texts := []string{rules.Text(seed)}
		available := []int64{}
		for _, c := range pool {
			if text := rules.SummaryText(c.item.Overview); text != "" {
				texts = append(texts, text)
				available = append(available, c.item.TMDBID)
			}
		}
		if vectors, e := encode(texts); e == nil {
			scores := map[int64]float64{}
			for i, id := range available {
				scores[id] = cosine(vectors[0], vectors[i+1])
			}
			selected = slices.Clone(pool)
			sort.SliceStable(selected, func(i, j int) bool {
				left, lok := scores[selected[i].item.TMDBID]
				right, rok := scores[selected[j].item.TMDBID]
				if lok != rok {
					return lok
				}
				return left > right
			})
		} else {
			out.Warnings = append(out.Warnings, rules.Warning{Code: "semantic_selection_unavailable", Message: "Using provider order because local semantic selection is unavailable."})
		}
	}
	selected = selected[:min(ceiling, len(selected))]
	type checked struct {
		facts rules.Facts
		err   error
	}
	facts := make([]checked, len(selected))
	jobs := make(chan int, len(selected))
	for i := range selected {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for range min(4, len(selected)) {
		wg.Go(func() {
			for i := range jobs {
				facts[i].facts, facts[i].err = s.source.Facts(pctx, selected[i].item.TMDBID)
			}
		})
	}
	wg.Wait()
	for i, c := range selected {
		f := facts[i].facts
		if facts[i].err != nil {
			partial()
			continue
		}
		out.CheckedCount++
		e, ok := rules.Eligible(f, a.Filters)
		if !ok || domain.ExternalIDsConflict(f.IDs, domain.ExternalIDs{TMDB: c.item.TMDBID}) || seed.IDs.TVDB > 0 && seed.IDs.TVDB == f.IDs.TVDB {
			continue
		}
		item := ports.SearchResult{Kind: domain.KindSeries, TMDBID: f.IDs.TMDB, TVDBID: f.IDs.TVDB, IMDBID: f.IDs.IMDB, Title: f.Title, Year: f.Year, Overview: f.Overview, PosterPath: f.PosterPath, Source: "tmdb", HydrationSource: "tmdb"}
		out.Results = append(out.Results, Result{Item: item, Facts: f, Evidence: e, Metadata: rr(c)})
	}
	if out.CheckedCount == 0 && len(selected) > 0 {
		return out, ErrUnavailable
	}
	thin := a.Seed != nil && !rules.UsableSeed(seed)
	if thin {
		out.Warnings = append(out.Warnings, rules.Warning{Code: "seed_similarity_unavailable", Message: "The seed has too little description for semantic comparison."})
	}
	if state == "ready" && len(out.Results) > 0 && (!thin || a.Filters.Theme != nil) {
		texts := []string{}
		qIndex, sIndex := -1, -1
		if a.RankingText != "" {
			qIndex = len(texts)
			texts = append(texts, a.RankingText)
		}
		if a.Seed != nil && !thin {
			sIndex = len(texts)
			texts = append(texts, rules.Text(seed))
		}
		offset := len(texts)
		for _, r := range out.Results {
			texts = append(texts, rules.Text(r.Facts))
		}
		if v, e := encode(texts); e == nil && offset > 0 {
			out.Ranking = "semantic"
			out.SeedCompared = sIndex >= 0
			ranked := out.Results[:0]
			for i, r := range out.Results {
				q, ss := 0., 0.
				if qIndex >= 0 {
					q = cosine(v[qIndex], v[offset+i])
				}
				if sIndex >= 0 {
					ss = cosine(v[sIndex], v[offset+i])
				}
				if a.Filters.Theme == nil && sIndex >= 0 && ss < .48 {
					continue
				}
				r.Score = q
				if qIndex >= 0 && sIndex >= 0 {
					r.Score = .6*q + .4*ss
				} else if sIndex >= 0 {
					r.Score = ss
				}
				ranked = append(ranked, r)
			}
			out.Results = ranked
		} else {
			out.Warnings = append(out.Warnings, rules.Warning{Code: "semantic_ranking_unavailable", Message: "Local semantic ranking is unavailable; results use provider ordering."})
		}
	}
	modelState, current := s.model()
	out.ModelState = modelState
	if current != generation {
		return out, ErrStateChanged
	}
	if out.Coverage == "partial" && len(out.Results) == 0 {
		return out, ErrUnavailable
	}
	if out.Ranking == "metadata_only" {
		if a.Seed != nil {
			out.Warnings = append(out.Warnings, rules.Warning{Code: "provider_suggestions_only", Message: "Related results use provider suggestions."})
		}
		if a.Query != "" {
			out.Warnings = append(out.Warnings, rules.Warning{Code: "prose_ranking_unavailable", Message: "Extra description preferences require local semantic ranking."})
		}
	}
	sort.SliceStable(out.Results, func(i, j int) bool {
		a, b := out.Results[i], out.Results[j]
		if a.Evidence.State != b.Evidence.State {
			return a.Evidence.State == "central"
		}
		if out.Ranking == "semantic" && a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Metadata != b.Metadata {
			return a.Metadata > b.Metadata
		}
		return a.Item.TMDBID < b.Item.TMDBID
	})
	out.EligibleCount = len(out.Results)
	_, after, e := s.source.Snapshot(ctx)
	if e != nil || after != provider {
		return out, ErrStateChanged
	}
	return out, nil
}

func (s *Service) publish(ctx context.Context, value Response, provider string, generation uint64) (Response, error) {
	value = s.own(ctx, value)
	_, current, err := s.source.Snapshot(ctx)
	_, model := s.model()
	if err != nil || current != provider || model != generation {
		return Response{}, ErrStateChanged
	}
	return value, nil
}
func cosine(a, b []float32) float64 {
	sum := 0.
	for i := range min(len(a), len(b)) {
		sum += float64(a[i] * b[i])
	}
	return sum
}
func (s *Service) own(ctx context.Context, value Response) Response {
	out := value
	out.Results = slices.Clone(value.Results)
	out.Warnings = slices.Clone(value.Warnings)
	ids := []domain.ExternalIDs{}
	for _, r := range out.Results {
		ids = append(ids, r.Facts.IDs)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	states, err := s.ownership.LookupSeries(ctx, ids)
	visible := out.Results[:0]
	for i, r := range out.Results {
		r.Ownership = ports.Ownership{State: "unknown"}
		if err == nil && i < len(states) {
			r.Ownership = states[i]
		}
		if out.Applied.Filters.HideInLibrary && r.Ownership.State == "present" {
			out.HiddenOwnedCount++
			continue
		}
		visible = append(visible, r)
	}
	out.Results = visible[:min(out.Applied.Limit, len(visible))]
	if err != nil {
		out.Warnings = append(out.Warnings, rules.Warning{Code: "ownership_unavailable", Message: "Library ownership could not be checked; Add will verify it."})
	}
	return out
}
