package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// ---- wanted search: conflict error, results paging, job wiring, coordinator ----

func TestGapAcqWantedSearchConflictUnwrapsToTheSentinel(t *testing.T) {
	conflict := &WantedSearchConflict{ActiveRunID: "abc", Message: "busy"}
	if conflict.Error() != "busy" {
		t.Fatalf("Error() = %q", conflict.Error())
	}
	var err error = conflict
	if !errors.Is(err, ErrWantedSearchConflict) {
		t.Fatal("conflict does not unwrap to ErrWantedSearchConflict")
	}

	// And the real thing: a second, different scope while one run is active.
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	ctx := context.Background()
	first, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "all"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "reason", Reason: WantedMissing})
	var got *WantedSearchConflict
	if !errors.As(err, &got) || got.ActiveRunID != first.RunID || !errors.Is(err, ErrWantedSearchConflict) {
		t.Fatalf("second scope = %v, want conflict naming run %s", err, first.RunID)
	}
	// The same scope again is idempotent rather than a conflict.
	again, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "all"})
	if err != nil || again.RunID != first.RunID {
		t.Fatalf("same scope = %+v, %v; want run %s reused", again, err, first.RunID)
	}
}

// gapTwoMovieRun starts a two-target run with no per-target delay and returns
// the service, db and the stored run.
func gapTwoMovieRun(t *testing.T) (*Service, *sqlite.DB, sqlite.WantedSearchRun) {
	t.Helper()
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	if _, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Second Movie", SortTitle: "second movie",
		Year: 2025, IDs: domain.ExternalIDs{TMDB: 987654}, Monitored: true, Path: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	delay := 0
	run, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "all", TargetDelay: &delay})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetWantedSearch(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Selected != 2 || stored.Cursor != 0 {
		t.Fatalf("fixture run = %+v, want two pending targets", stored)
	}
	return svc, db, stored
}

// gapRunChunk claims the next queued chunk, runs it and completes the job.
func gapRunChunk(t *testing.T, svc *Service, db *sqlite.DB) domain.Job {
	t.Helper()
	ctx := context.Background()
	job, err := db.ClaimJob(ctx, "test", nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.handleWantedSearchChunk(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestGapAcqWantedSearchResultsPageThroughCommittedTargets(t *testing.T) {
	svc, db, run := gapTwoMovieRun(t)
	ctx := context.Background()

	// Nothing has been processed: the page is empty but the run is known.
	rows, err := svc.WantedSearchResults(ctx, run.RunID, 0, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("results before processing = %+v, %v", rows, err)
	}
	gapRunChunk(t, svc, db)
	if err := svc.ReconcileWantedSearches(ctx); err != nil {
		t.Fatal(err)
	}
	gapRunChunk(t, svc, db)

	all, err := svc.WantedSearchResults(ctx, run.RunID, 0, -5) // clamped to defaults
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Ordinal != 0 || all[1].Ordinal != 1 {
		t.Fatalf("all results = %+v", all)
	}
	for _, r := range all {
		if r.State != "searched" || r.Label == "" || r.WantableID == "" || r.FinishedAt.IsZero() {
			t.Fatalf("result %+v is not a finished searched target", r)
		}
	}
	first, err := svc.WantedSearchResults(ctx, run.RunID, 1, 0)
	if err != nil || len(first) != 1 || first[0].Ordinal != 0 {
		t.Fatalf("page 1 = %+v, %v", first, err)
	}
	second, err := svc.WantedSearchResults(ctx, run.RunID, 1, 1)
	if err != nil || len(second) != 1 || second[0].Ordinal != 1 {
		t.Fatalf("page 2 = %+v, %v", second, err)
	}
	if second[0].WantableID == first[0].WantableID {
		t.Fatal("paging returned the same target twice")
	}
	huge, err := svc.WantedSearchResults(ctx, run.RunID, 5000, 0) // over the cap: default page
	if err != nil || len(huge) != 2 {
		t.Fatalf("capped page = %+v, %v", huge, err)
	}
	if _, err := svc.WantedSearchResults(ctx, "no-such-run", 10, 0); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("unknown run = %v, want ErrNotFound", err)
	}
}

func TestGapAcqRegisterWantedSearchJobsWiresTheChunkHandler(t *testing.T) {
	type handler func(context.Context, domain.Job) error
	svc, db, run := gapTwoMovieRun(t)
	ctx := context.Background()

	var kind string
	var h handler
	err := RegisterWantedSearchJobs(svc, func(k string, fn handler) error {
		kind, h = k, fn
		return nil
	})
	if err != nil || kind != WantedSearchJobKind || h == nil {
		t.Fatalf("register = %v kind=%q handler=%v", err, kind, h != nil)
	}
	boom := errors.New("registry full")
	if err := RegisterWantedSearchJobs(svc, func(string, handler) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("register error = %v, want %v", err, boom)
	}

	if err := h(ctx, domain.Job{Payload: "{not json"}); err == nil || !strings.Contains(err.Error(), "payload") {
		t.Fatalf("malformed payload = %v", err)
	}
	if err := h(ctx, domain.Job{Payload: `{"runId":"","ordinal":0}`}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete payload = %v", err)
	}
	if err := h(ctx, domain.Job{Payload: `{"runId":"missing","ordinal":0}`}); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	// A stale chunk for an ordinal the run no longer owns is a no-op.
	if err := h(ctx, domain.Job{Payload: `{"runId":"` + run.RunID + `","ordinal":7}`}); err != nil {
		t.Fatalf("stale ordinal = %v", err)
	}
	// Once cancellation is requested the chunk does nothing and leaves the
	// run for reconciliation to terminalise.
	if _, err := db.RequestWantedSearchCancel(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	if err := h(ctx, domain.Job{Payload: `{"runId":"` + run.RunID + `","ordinal":0}`}); err != nil {
		t.Fatalf("cancelled chunk = %v", err)
	}
	if err := svc.ReconcileWantedSearches(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetWantedSearch(ctx, run.RunID)
	if err != nil || got.Status != "cancelled" || got.Searched != 0 || got.Skipped != 2 {
		t.Fatalf("after cancelled chunk = %+v, %v; want every target skipped, none searched", got, err)
	}
	// A terminal run is ignored by the handler.
	if err := h(ctx, domain.Job{Payload: `{"runId":"` + run.RunID + `","ordinal":0}`}); err != nil {
		t.Fatalf("terminal run chunk = %v", err)
	}
}

func TestGapAcqRunWantedSearchCoordinatorDrainsAQueuedRun(t *testing.T) {
	svc, db, run := gapTwoMovieRun(t)
	ctx := context.Background()

	first := gapRunChunk(t, svc, db)
	// No reconcile here: the second chunk is not enqueued until the
	// coordinator's first pass does it.
	if _, err := db.FindNewestJobByDedupe(ctx, "wanted.search:"+run.RunID); err != nil {
		t.Fatal(err)
	}
	coordCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunWantedSearchCoordinator(coordCtx)
	}()

	waitFor := func(what string, timeout time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for !ok() {
			if time.Now().After(deadline) {
				stop()
				t.Fatalf("coordinator never %s", what)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	waitFor("enqueued the second chunk", 5*time.Second, func() bool {
		job, err := db.FindNewestJobByDedupe(ctx, "wanted.search:"+run.RunID)
		return err == nil && job.ID != first.ID && job.State == domain.JobQueued
	})
	gapRunChunk(t, svc, db)
	// The run is fully processed but only a reconcile (the ticker's) completes it.
	waitFor("completed the drained run", 8*time.Second, func() bool {
		got, err := db.GetWantedSearch(ctx, run.RunID)
		return err == nil && got.Status == "completed"
	})
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("coordinator did not stop on context cancellation")
	}
	got, err := svc.WantedSearch(ctx, run.RunID)
	if err != nil || got.Processed != 2 || got.Searched != 2 || got.Status != "completed" {
		t.Fatalf("final run = %+v, %v", got, err)
	}
}

// ---- executeWantedTarget: every skip and failure reason ----

// gapHookIndexer returns one matching release and runs a hook first, so a test
// can change the world between the search and the final pre-grab validation.
type gapHookIndexer struct {
	hook     func()
	releases []ports.Release
}

func (i *gapHookIndexer) Search(context.Context, domain.SearchQuery) ([]ports.Release, error) {
	if i.hook != nil {
		i.hook()
	}
	return i.releases, nil
}
func (i *gapHookIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (i *gapHookIndexer) Test(context.Context) error                        { return nil }

func TestGapAcqExecuteWantedTargetReportsEverySkipAndFailure(t *testing.T) {
	type scenario struct {
		name    string
		arrange func(t *testing.T, svc *Service, db *sqlite.DB, movieID int64, run sqlite.WantedSearchRun, target *sqlite.WantedSearchTarget)
		state   string
		skipped string
		errPart string
	}
	insertDownload := func(t *testing.T, db *sqlite.DB, movieID int64, state string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := db.InsertDownload(context.Background(), sqlite.Download{
				MediaItemID: movieID, WantableIDs: []string{"movie:" + itoa(movieID)},
				ReleaseTitle: "Row." + state + "." + itoa(int64(i)), Protocol: "torrent", State: state,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	unmonitor := func(t *testing.T, db *sqlite.DB, movieID int64) {
		t.Helper()
		if _, err := db.W.ExecContext(context.Background(),
			`UPDATE media_items SET monitored = 0 WHERE id = ?`, movieID); err != nil {
			t.Fatal(err)
		}
	}
	scenarios := []scenario{
		{
			name: "cancel already requested",
			arrange: func(t *testing.T, _ *Service, db *sqlite.DB, _ int64, run sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				if _, err := db.RequestWantedSearchCancel(context.Background(), run.RunID); err != nil {
					t.Fatal(err)
				}
			},
			state: "skipped", skipped: "cancelled", errPart: "cancellation was requested",
		},
		{
			name: "target deleted",
			arrange: func(_ *testing.T, _ *Service, _ *sqlite.DB, _ int64, _ sqlite.WantedSearchRun, target *sqlite.WantedSearchTarget) {
				target.WantableID = "movie:999999"
			},
			state: "skipped", skipped: "no_longer_wanted", errPart: "no longer exists",
		},
		{
			name: "target id unparseable",
			arrange: func(_ *testing.T, _ *Service, _ *sqlite.DB, _ int64, _ sqlite.WantedSearchRun, target *sqlite.WantedSearchTarget) {
				target.WantableID = "garbage"
			},
			state: "skipped", skipped: "no_longer_wanted", errPart: "unparseable",
		},
		{
			name: "target unmonitored",
			arrange: func(t *testing.T, _ *Service, db *sqlite.DB, movieID int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				unmonitor(t, db, movieID)
			},
			state: "skipped", skipped: "unmonitored", errPart: "no longer monitored",
		},
		{
			name: "reason changed since snapshot",
			arrange: func(_ *testing.T, _ *Service, _ *sqlite.DB, _ int64, _ sqlite.WantedSearchRun, target *sqlite.WantedSearchTarget) {
				target.SelectedReason = string(WantedUpgrade)
			},
			state: "skipped", skipped: "reason_changed", errPart: "target is now missing",
		},
		{
			name: "already downloading",
			arrange: func(t *testing.T, _ *Service, db *sqlite.DB, movieID int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				insertDownload(t, db, movieID, "grabbed", 1)
			},
			state: "skipped", skipped: "downloading", errPart: "download in progress",
		},
		{
			name: "regrab capped",
			arrange: func(t *testing.T, _ *Service, db *sqlite.DB, movieID int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				insertDownload(t, db, movieID, "failed", reGrabLimit)
			},
			state: "skipped", skipped: "regrab_capped", errPart: "re-grab limit",
		},
		{
			name: "no indexers left",
			arrange: func(t *testing.T, _ *Service, db *sqlite.DB, _ int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				indexers, _ := db.ListIndexers(context.Background())
				for _, ic := range indexers {
					if err := db.DeleteIndexer(context.Background(), ic.ID); err != nil {
						t.Fatal(err)
					}
				}
			},
			state: "failed", errPart: ErrNoIndexers.Error(),
		},
		{
			name: "cancelled before grab",
			arrange: func(t *testing.T, svc *Service, db *sqlite.DB, _ int64, run sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				svc.newIndexer = func(ports.IndexerConfig) ports.Indexer {
					return &gapHookIndexer{
						releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 20)},
						hook: func() {
							if _, err := db.RequestWantedSearchCancel(context.Background(), run.RunID); err != nil {
								t.Error(err)
							}
						},
					}
				}
			},
			state: "skipped", skipped: "cancelled",
		},
		{
			name: "unmonitored before grab",
			arrange: func(t *testing.T, svc *Service, db *sqlite.DB, movieID int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				svc.newIndexer = func(ports.IndexerConfig) ports.Indexer {
					return &gapHookIndexer{
						releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 20)},
						hook:     func() { unmonitor(t, db, movieID) },
					}
				}
			},
			state: "skipped", skipped: "no_longer_wanted", errPart: "before search or grab",
		},
		{
			name: "download appeared before grab",
			arrange: func(t *testing.T, svc *Service, db *sqlite.DB, movieID int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				svc.newIndexer = func(ports.IndexerConfig) ports.Indexer {
					return &gapHookIndexer{
						releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 20)},
						hook:     func() { insertDownload(t, db, movieID, "grabbed", 1) },
					}
				}
			},
			state: "skipped", skipped: "downloading",
		},
		{
			name: "regrab cap reached before grab",
			arrange: func(t *testing.T, svc *Service, db *sqlite.DB, movieID int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				svc.newIndexer = func(ports.IndexerConfig) ports.Indexer {
					return &gapHookIndexer{
						releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 20)},
						hook:     func() { insertDownload(t, db, movieID, "failed", reGrabLimit) },
					}
				}
			},
			state: "skipped", skipped: "regrab_capped",
		},
		{
			name: "searched and grabbed",
			arrange: func(_ *testing.T, svc *Service, _ *sqlite.DB, _ int64, _ sqlite.WantedSearchRun, _ *sqlite.WantedSearchTarget) {
				svc.newIndexer = func(ports.IndexerConfig) ports.Indexer {
					return &gapHookIndexer{releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 20)}}
				}
			},
			state: "searched",
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			client := &fakeClient{}
			svc, db, movieID := autoSetup(t, nil, client)
			svc.WithWantedSearchQueue(wantedDBQueue{db: db})
			ctx := context.Background()
			delay := 0
			started, err := svc.StartWantedSearch(ctx, WantedSearchRequest{
				Scope: "target", WantableID: "movie:" + itoa(movieID), TargetDelay: &delay,
			})
			if err != nil {
				t.Fatal(err)
			}
			run, err := db.GetWantedSearch(ctx, started.RunID)
			if err != nil {
				t.Fatal(err)
			}
			target, err := db.WantedSearchTargetAt(ctx, run.RunID, 0)
			if err != nil {
				t.Fatal(err)
			}
			sc.arrange(t, svc, db, movieID, run, &target)

			got := svc.executeWantedTarget(ctx, run, target)
			if got.State != sc.state || got.Skipped != sc.skipped {
				t.Fatalf("result = %+v, want state %q skipped %q", got, sc.state, sc.skipped)
			}
			if sc.errPart != "" && !strings.Contains(got.Error, sc.errPart) {
				t.Fatalf("error = %q, want it to mention %q", got.Error, sc.errPart)
			}
			if sc.state == "searched" {
				if got.Grabbed == "" || got.Seen != 1 || got.Matched != 1 || got.Accepted != 1 || len(client.added) != 1 {
					t.Fatalf("searched result = %+v grabs=%v", got, client.added)
				}
			} else if len(client.added) != 0 {
				t.Fatalf("a %s target was grabbed: %v", sc.skipped, client.added)
			}
		})
	}
}

// ---- indexer search: capability-driven fallback and error handling ----

type gapCapsIndexer struct {
	caps        ports.IndexerCapabilities
	capsErr     error
	search      func(domain.SearchQuery) ([]ports.Release, error)
	mu          sync.Mutex
	queries     []domain.SearchQuery
	invalidated []string
}

func (i *gapCapsIndexer) Capabilities(context.Context) (ports.IndexerCapabilities, error) {
	return i.caps, i.capsErr
}
func (i *gapCapsIndexer) Search(_ context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	i.mu.Lock()
	i.queries = append(i.queries, q)
	i.mu.Unlock()
	return i.search(q)
}
func (i *gapCapsIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (i *gapCapsIndexer) Test(context.Context) error                        { return nil }
func (i *gapCapsIndexer) InvalidateSearchCapability(mode string) {
	i.mu.Lock()
	i.invalidated = append(i.invalidated, mode)
	i.mu.Unlock()
}

func gapTVCaps(params ...string) ports.IndexerCapabilities {
	p := map[string]bool{}
	for _, k := range params {
		p[k] = true
	}
	return ports.IndexerCapabilities{
		Generic: ports.IndexerSearchCapability{Known: true, Available: true, Parameters: map[string]bool{"q": true}},
		TV:      ports.IndexerSearchCapability{Known: true, Available: true, Parameters: p},
	}
}

func gapEpisode() domain.EpisodeWantable {
	return domain.EpisodeWantable{Item: 1, Season: 2, Episode: 3, Mon: true, Identity: domain.MediaIdentity{
		Title: "Gap Show", IDs: domain.ExternalIDs{TVDB: 4242},
	}}
}

func TestGapAcqApplyCoverageCapabilitiesDropsUnsupportedSeasonAndEpisode(t *testing.T) {
	cases := []struct {
		name               string
		params             []string
		season, episodeSet bool
	}{
		{"q only", []string{"q"}, false, false},
		{"q and season", []string{"q", "season"}, true, false},
		{"q season ep", []string{"q", "season", "ep"}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queries, _, err := queriesForIndexer(context.Background(),
				&gapCapsIndexer{caps: gapTVCaps(tc.params...)}, gapEpisode(), false)
			if err != nil || len(queries) == 0 {
				t.Fatalf("queries = %+v, %v", queries, err)
			}
			q := queries[len(queries)-1] // the title query, in tv mode
			if q.Mode != "tv" || q.SeasonSet != tc.season || q.EpisodeSet != tc.episodeSet {
				t.Fatalf("query = %+v, want tv mode season=%v ep=%v", q, tc.season, tc.episodeSet)
			}
		})
	}
}

func TestGapAcqGenericCanonicalQueryNeedsAVideoTitle(t *testing.T) {
	if q, ok := genericCanonicalQuery(domain.BookWantable{Item: 1, Title: "Some Book"}); ok {
		t.Fatalf("a book produced a generic video query: %+v", q)
	}
	q, ok := genericCanonicalQuery(gapEpisode())
	if !ok || q.Mode != "generic" || q.SeasonSet || q.EpisodeSet || !strings.Contains(q.Q, "Gap Show") {
		t.Fatalf("generic query = %+v ok=%v", q, ok)
	}
}

func TestGapAcqExecuteIndexerSearchHandlesEveryFailureShape(t *testing.T) {
	svc := &Service{searchTimeout: 5 * time.Second}
	unsupported := &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	transport := &ports.RemoteError{Category: ports.RemoteTransport}
	release := ports.Release{Title: "Gap.Show.S02E03.1080p.WEB-DL-GRP", DownloadURL: "u"}

	t.Run("unsupported tv mode falls back to a generic probe", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: gapTVCaps("q", "tvdbid", "season", "ep"), search: func(q domain.SearchQuery) ([]ports.Release, error) {
			if q.Mode == "generic" {
				return []ports.Release{release}, nil
			}
			return nil, unsupported
		}}
		got, reasons := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, nil)
		if len(got) != 1 || got[0].Title != release.Title {
			t.Fatalf("releases = %+v", got)
		}
		if len(reasons) != 1 || reasons[0] != "tier 1: unsupported_query" {
			t.Fatalf("reasons = %v", reasons)
		}
		if len(idx.invalidated) != 1 || idx.invalidated[0] != "tv" {
			t.Fatalf("invalidated = %v, want the tv mode downgraded", idx.invalidated)
		}
		if len(idx.queries) != 2 || idx.queries[0].Mode != "tv" || idx.queries[1].Mode != "generic" ||
			idx.queries[1].SeasonSet || idx.queries[1].EpisodeSet || idx.queries[1].ID != nil {
			t.Fatalf("queries = %+v, want one tv attempt then one canonical generic probe", idx.queries)
		}
	})

	t.Run("fallback failure is reported and the search stops", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: gapTVCaps("q", "tvdbid"), search: func(q domain.SearchQuery) ([]ports.Release, error) {
			if q.Mode == "generic" {
				return nil, transport
			}
			return nil, unsupported
		}}
		got, reasons := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, nil)
		if len(got) != 0 || len(reasons) != 2 || reasons[1] != "generic fallback: transport" {
			t.Fatalf("releases = %+v reasons = %v", got, reasons)
		}
		if len(idx.queries) != 2 {
			t.Fatalf("queries = %+v, want the tv id query and the fallback only", idx.queries)
		}
	})

	t.Run("stop predicate ends the search on fallback rows", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: gapTVCaps("q", "tvdbid"), search: func(q domain.SearchQuery) ([]ports.Release, error) {
			if q.Mode == "generic" {
				return []ports.Release{release}, nil
			}
			return nil, unsupported
		}}
		stops := 0
		got, _ := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, func(rs []ports.Release) bool {
			stops++
			return len(rs) > 0
		})
		if len(got) != 1 || stops != 1 {
			t.Fatalf("releases = %+v stops = %d", got, stops)
		}
	})

	t.Run("terminal auth error stops after the first tier", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: gapTVCaps("q", "tvdbid"), search: func(domain.SearchQuery) ([]ports.Release, error) {
			return nil, &ports.RemoteError{Category: ports.RemoteAuth, HTTPStatus: 401}
		}}
		got, reasons := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, nil)
		if len(got) != 0 || len(reasons) != 1 || reasons[0] != "tier 1: auth" || len(idx.queries) != 1 {
			t.Fatalf("releases = %+v reasons = %v queries = %d", got, reasons, len(idx.queries))
		}
	})

	t.Run("transient error continues to the next tier", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: gapTVCaps("q", "tvdbid"), search: func(q domain.SearchQuery) ([]ports.Release, error) {
			if q.ID != nil {
				return nil, transport
			}
			return []ports.Release{release}, nil
		}}
		got, reasons := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, nil)
		if len(got) != 1 || len(reasons) != 1 || reasons[0] != "tier 1: transport" || len(idx.queries) != 2 {
			t.Fatalf("releases = %+v reasons = %v queries = %d", got, reasons, len(idx.queries))
		}
	})

	t.Run("stop predicate ends the search on a good tier", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: gapTVCaps("q", "tvdbid"), search: func(domain.SearchQuery) ([]ports.Release, error) {
			return []ports.Release{release}, nil
		}}
		got, reasons := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, func([]ports.Release) bool { return true })
		if len(got) != 1 || len(reasons) != 0 || len(idx.queries) != 1 {
			t.Fatalf("releases = %+v reasons = %v queries = %d", got, reasons, len(idx.queries))
		}
	})

	t.Run("unsupported generic query has no fallback", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: ports.IndexerCapabilities{}, search: func(domain.SearchQuery) ([]ports.Release, error) {
			return nil, unsupported
		}}
		got, reasons := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, nil)
		if len(got) != 0 || len(reasons) != 1 || len(idx.queries) != 1 || len(idx.invalidated) != 0 {
			t.Fatalf("releases = %+v reasons = %v queries = %d invalidated = %v", got, reasons, len(idx.queries), idx.invalidated)
		}
	})

	t.Run("capability discovery failure is one reason", func(t *testing.T) {
		idx := &gapCapsIndexer{capsErr: context.DeadlineExceeded, search: func(domain.SearchQuery) ([]ports.Release, error) {
			t.Fatal("searched without capabilities")
			return nil, nil
		}}
		got, reasons := svc.executeIndexerSearch(context.Background(), idx, gapEpisode(), false, nil)
		if got != nil || len(reasons) != 1 || reasons[0] != "timeout" {
			t.Fatalf("releases = %+v reasons = %v", got, reasons)
		}
	})
}

// ---- cleanup: path boundaries and the imported-dir guard ----

func TestGapAcqWithinRespectsPathBoundaries(t *testing.T) {
	cases := []struct {
		parent, child string
		want          bool
	}{
		{"/data/media", "/data/media/show", true},
		{"/data/media", "/data/media", true},
		{"/data/media", "/data/media-old", false},
		{"/data/media", "/data", false},
		{"/data/media", "/data/other/x", false},
		{"/data/media", "relative", false},
	}
	for _, tc := range cases {
		if got := within(tc.parent, tc.child); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}

func TestGapAcqRemoveImportedDirOnlyRemovesAPayloadOutsideEveryRoot(t *testing.T) {
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients = %+v, %v", clients, err)
	}
	client := clients[0]
	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddRootFolder(ctx, root, domain.KindMixed); err != nil {
		t.Fatal(err)
	}
	mkdir := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exists := func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}
	newRow := func(handle string) int64 {
		t.Helper()
		id, err := db.InsertDownload(ctx, sqlite.Download{
			MediaItemID: itemID, ReleaseTitle: "Payload." + handle, Protocol: "usenet",
			ClientID: client.ID, Handle: handle, State: "imported",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pending := func() int {
		rows, err := db.ImportedWithPayload(ctx, 50)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}

	// The operator has not asked for payload removal: nothing is touched.
	outside := mkdir(filepath.Join(base, "completed", "Some.Release"))
	svc.removeImportedDir(ctx, downloadRef{ID: newRow("h-off"), MediaItemID: itemID, ClientID: client.ID, ImportPath: outside})
	if !exists(outside) {
		t.Fatal("a payload was removed while RemoveCompleted was off")
	}

	client.RemoveCompleted = true
	if err := db.UpdateDownloadClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	before := pending()
	// Malformed or dangerous targets are ignored without consulting anything.
	for _, bad := range []string{"", "   ", "relative/dir", "/"} {
		svc.removeImportedDir(ctx, downloadRef{ID: 1, ClientID: client.ID, ImportPath: bad})
	}
	// An unknown client is treated as "leave it alone".
	svc.removeImportedDir(ctx, downloadRef{ID: 1, ClientID: 999, ImportPath: outside})
	if !exists(outside) {
		t.Fatal("an unknown client's payload was removed")
	}
	// Inside a root folder: refused.
	inside := mkdir(filepath.Join(root, "Show", "Payload"))
	svc.removeImportedDir(ctx, downloadRef{ID: newRow("h-in"), ClientID: client.ID, ImportPath: inside})
	if !exists(inside) {
		t.Fatal("a directory inside a root folder was removed")
	}
	// A root folder itself, or a parent of one: refused.
	svc.removeImportedDir(ctx, downloadRef{ID: newRow("h-root"), ClientID: client.ID, ImportPath: root})
	svc.removeImportedDir(ctx, downloadRef{ID: newRow("h-parent"), ClientID: client.ID, ImportPath: base})
	if !exists(root) || !exists(base) {
		t.Fatal("a root folder or its parent was removed")
	}
	// A sibling with a shared prefix is not inside the root, but if it does
	// not exist there is nothing to do and nothing to record.
	svc.removeImportedDir(ctx, downloadRef{ID: newRow("h-missing"), ClientID: client.ID, ImportPath: root + "-old"})
	if pending() != before+4 {
		t.Fatalf("refused removals marked a payload as removed: pending %d, want %d", pending(), before+4)
	}

	// The real payload, outside every root: removed and recorded.
	id := newRow("h-yes")
	if err := os.WriteFile(filepath.Join(outside, "file.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.removeImportedDir(ctx, downloadRef{ID: id, MediaItemID: itemID, ClientID: client.ID,
		ImportPath: outside, ReleaseTitle: "Payload.h-yes", Size: 12})
	if exists(outside) {
		t.Fatal("the completed payload directory survived")
	}
	if pending() != before+4 {
		t.Fatalf("the removal was not recorded on the row: pending %d, want %d", pending(), before+4)
	}
	history, err := db.ListHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range history {
		if h.Type == HistoryPayloadRemoved && h.ReleaseTitle == "Payload.h-yes" && strings.Contains(h.Data, outside) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s history for the removed payload: %+v", HistoryPayloadRemoved, history)
	}
}

// ---- manual import: default path discovery and restart recovery ----

// gapInsertDownloadWithPaths inserts a row and persists its save/import
// paths, which InsertDownload deliberately leaves to the handoff writer.
func gapInsertDownloadWithPaths(t *testing.T, db *sqlite.DB, dl sqlite.Download) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := db.InsertDownload(ctx, dl)
	if err != nil {
		t.Fatal(err)
	}
	dl.ID = id
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGapAcqManualImportDefaultPathLearnsFromRecentPayloads(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients = %+v, %v", clients, err)
	}
	// A disabled client's mapping teaches nothing, nor does a blank one.
	disabled := clients[0]
	disabled.Enabled = false
	disabled.PathMappings = []ports.PathMapping{{Remote: "/remote", Local: "/ignored/completed"}}
	if err := db.UpdateDownloadClient(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "sabnzbd", Name: "sab", URL: "http://sab",
		Enabled: true, PathMappings: []ports.PathMapping{{Remote: "/remote", Local: "   "}}}); err != nil {
		t.Fatal(err)
	}
	if got := svc.ManualImportDefaultPath(ctx); got != "/pool/downloads" {
		t.Fatalf("with nothing to learn from = %q, want the deployment default", got)
	}

	gapInsertDownloadWithPaths(t, db, sqlite.Download{
		MediaItemID: movieID, ReleaseTitle: "Older", Protocol: "usenet", State: "imported",
		SavePath: "/client/completed/Older",
	})
	if got := svc.ManualImportDefaultPath(ctx); got != "/client/completed" {
		t.Fatalf("from the client's save path = %q", got)
	}
	time.Sleep(5 * time.Millisecond) // a later added_at, so this row is the newest
	gapInsertDownloadWithPaths(t, db, sqlite.Download{
		MediaItemID: movieID, ReleaseTitle: "Newer", Protocol: "usenet", State: "imported",
		SavePath: "/client/completed/Newer", ImportPath: "/mapped/completed/Newer/",
	})
	if got := svc.ManualImportDefaultPath(ctx); got != "/mapped/completed" {
		t.Fatalf("from the newest mapped import path = %q", got)
	}
}

func TestGapAcqRecoverManualImportsRequeuesInterruptedStandaloneJobs(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	clients, _ := db.ListDownloadClients(ctx)

	interrupted := gapInsertDownloadWithPaths(t, db, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: epRelease, Protocol: "manual",
		State: "importing", ImportPath: payloadDir(t),
	})
	waiting := gapInsertDownloadWithPaths(t, db, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E02.1080p.WEB-DL.x264-GRP", Protocol: "manual",
		State: "downloaded", ImportPath: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	// A client-backed row is the client's to report again; a manual row that
	// is not in an import state is not recovery's business either.
	clientBacked, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Client.Row", Protocol: "torrent",
		ClientID: clients[0].ID, Handle: "h-client", State: "importing",
	})
	if err != nil {
		t.Fatal(err)
	}
	grabbedManual, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "Manual.Grabbed", Protocol: "manual", State: "grabbed",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.recoverManualImports(ctx) // inline: no workers are running

	got, err := db.GetDownload(ctx, interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "imported" {
		t.Fatalf("interrupted manual import = %q (%s), want imported", got.State, got.Error)
	}
	recovered := false
	for _, h := range got.Handoff {
		recovered = recovered || h.Step == stepImportRecovered
	}
	if !recovered {
		t.Fatalf("no %s step on the recovered row: %+v", stepImportRecovered, got.Handoff)
	}
	got, err = db.GetDownload(ctx, waiting)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == "downloaded" {
		t.Fatalf("the waiting manual row was not attempted: %+v", got)
	}
	for _, h := range got.Handoff {
		if h.Step == stepImportRecovered {
			t.Fatalf("a row that never started got a recovery step: %+v", got.Handoff)
		}
	}
	for _, id := range []int64{clientBacked, grabbedManual} {
		row, err := db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != "importing" && row.State != "grabbed" {
			t.Fatalf("recovery touched row %d: %+v", id, row)
		}
		if len(row.Handoff) != 0 {
			t.Fatalf("recovery wrote handoff on row %d: %+v", id, row.Handoff)
		}
	}
}

// ---- books: the imported medium and the multipart upgrade plan ----

func TestGapAcqImportedBookMediumPrefersTheTargetThenTheFile(t *testing.T) {
	item := domain.MediaItem{ID: 3, BookType: quality.BookTypeAudiobook, Copies: []domain.MediaCopy{
		{ID: 7, BookType: quality.BookTypeEbook},
		{ID: 8, BookType: "unknown"},
	}}
	cases := []struct {
		name string
		item domain.MediaItem
		dl   sqlite.Download
		want string
	}{
		{"primary declares its type", item, sqlite.Download{}, "audiobook"},
		{"copy declares its type", item, sqlite.Download{CopyID: 7}, "ebook"},
		{"copy with junk type falls back to the file", item, sqlite.Download{CopyID: 8, Quality: quality.Quality{Source: quality.SourceEPUB}}, "ebook"},
		{"legacy primary falls back to the file", domain.MediaItem{ID: 4}, sqlite.Download{Quality: quality.Quality{Source: quality.SourceEPUB}}, "ebook"},
		{"nothing known", domain.MediaItem{ID: 4}, sqlite.Download{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := importedBookMedium(tc.item, tc.dl); got != tc.want {
				t.Fatalf("importedBookMedium = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGapAcqPlanBookImportRefusesASidegradeUnlessManual(t *testing.T) {
	svc, db, bookID := bookSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	if _, err := db.W.ExecContext(ctx,
		`UPDATE media_items SET book_type = 'audiobook', quality_profile_id = ? WHERE id = ?`,
		quality.AudiobookProfileID, bookID); err != nil {
		t.Fatal(err)
	}
	payload := func() string {
		dir := t.TempDir()
		for _, name := range []string{"01-opening.mp3", "02-ending.mp3"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	dl := sqlite.Download{MediaItemID: bookID, ReleaseTitle: "Andy Weir - Project Hail Mary MP3", Indexer: "idx"}

	res, err := svc.importDownload(ctx, dl, payload(), false)
	if err != nil || res.Imported != 2 {
		t.Fatalf("first multipart import = %+v, %v", res, err)
	}
	// The same quality again is not an upgrade: automation refuses it and
	// says so for every track, before placing anything.
	res, err = svc.importDownload(ctx, dl, payload(), false)
	if err == nil || !strings.Contains(err.Error(), "does not improve") {
		t.Fatalf("automatic sidegrade = %+v, %v", res, err)
	}
	if len(res.Files) != 2 || !strings.Contains(res.Files[1].Reason, "does not improve") {
		t.Fatalf("per-file outcomes = %+v", res.Files)
	}
	if files, _ := db.ListFilesForItem(ctx, bookID); len(files) != 2 {
		t.Fatalf("the refused import placed files: %+v", files)
	}
	// A person pointing at it is the override.
	res, err = svc.importDownload(ctx, dl, payload(), true)
	if err != nil || res.Imported != 2 {
		t.Fatalf("manual sidegrade = %+v, %v", res, err)
	}
}

// ---- wanted search: start-time validation, scope labels, missing queue ----

func TestGapAcqStartWantedSearchValidatesDelayGroupAndUnwantedTarget(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	ctx := context.Background()

	tooLong := int(MaxWantedTargetDelay/time.Millisecond) + 1
	if _, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "all", TargetDelay: &tooLong}); !errors.Is(err, ErrInvalidWantedSearch) {
		t.Fatalf("over-long delay = %v, want ErrInvalidWantedSearch", err)
	}
	if _, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "group", MediaItemID: 424242}); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("group for an unknown item = %v, want ErrNotFound", err)
	}

	// A group scope with a reason is labelled after both.
	run, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "group", MediaItemID: movieID, Reason: WantedMissing})
	if err != nil {
		t.Fatal(err)
	}
	if run.ScopeLabel != "Missing in Test Movie" || run.Selected != 1 {
		t.Fatalf("group run = %+v", run)
	}
	if _, err := svc.CancelWantedSearch(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	run, err = svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "group", MediaItemID: movieID})
	if err != nil {
		t.Fatal(err)
	}
	if run.ScopeLabel != "Test Movie" {
		t.Fatalf("group run without reason = %+v", run)
	}
	if _, err := svc.CancelWantedSearch(ctx, run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelWantedSearch(ctx, "no-such-run"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("cancel unknown = %v", err)
	}

}

func TestGapAcqStartWantedSearchAcceptsAResolvableTargetThatIsNotWanted(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	ctx := context.Background()
	// A target that is not on the wanted list right now is still accepted
	// when it resolves to exactly that id; execution then reports why it
	// was not searched instead of the start refusing it.
	if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET monitored = 0 WHERE id = ?`, movieID); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()
	delay := 0
	run, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "target", WantableID: "movie:" + itoa(movieID), TargetDelay: &delay})
	if err != nil {
		t.Fatal(err)
	}
	if run.Selected != 1 || run.ScopeLabel == "" {
		t.Fatalf("unmonitored target run = %+v", run)
	}
	gapRunChunk(t, svc, db)
	results, err := svc.WantedSearchResults(ctx, run.RunID, 10, 0)
	if err != nil || len(results) != 1 || results[0].Skipped != "unmonitored" {
		t.Fatalf("unmonitored target results = %+v, %v", results, err)
	}
}

func TestGapAcqWantedSearchWithoutAQueueIsAcceptedButNeedsRecovery(t *testing.T) {
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	run, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "all"})
	if err != nil {
		t.Fatalf("a missing queue must not refuse the run: %v", err)
	}
	if run.Status != "queued" || run.Selected != 1 {
		t.Fatalf("run without queue = %+v", run)
	}
	if err := svc.ReconcileWantedSearches(ctx); err == nil || !strings.Contains(err.Error(), "queue is not configured") {
		t.Fatalf("reconcile without queue = %v", err)
	}
	if _, err := db.FindNewestJobByDedupe(ctx, "wanted.search:"+run.RunID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("a job appeared without a queue: %v", err)
	}
	// The coordinator keeps trying on its ticker and survives the failure.
	coordCtx, stop := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunWantedSearchCoordinator(coordCtx)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("coordinator did not return after its context expired")
	}
	got, err := svc.WantedSearch(ctx, run.RunID)
	if err != nil || got.Status != "queued" {
		t.Fatalf("run after failed reconciles = %+v, %v; want still queued", got, err)
	}
}

// gapSlowIndexer blocks until its context is cancelled and records how long
// that took.
type gapSlowIndexer struct {
	mu      sync.Mutex
	waited  time.Duration
	started chan struct{}
}

func (i *gapSlowIndexer) Search(ctx context.Context, _ domain.SearchQuery) ([]ports.Release, error) {
	begin := time.Now()
	close(i.started)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
	}
	i.mu.Lock()
	i.waited = time.Since(begin)
	i.mu.Unlock()
	return nil, ctx.Err()
}
func (i *gapSlowIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (i *gapSlowIndexer) Test(context.Context) error                        { return nil }

func TestGapAcqExecuteWantedTargetCancellationInterruptsASlowSearch(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	ctx := context.Background()
	slow := &gapSlowIndexer{started: make(chan struct{})}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return slow }
	delay := 0
	started, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "target", WantableID: "movie:" + itoa(movieID), TargetDelay: &delay})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.GetWantedSearch(ctx, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := db.WantedSearchTargetAt(ctx, run.RunID, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-slow.started
		if _, err := db.RequestWantedSearchCancel(context.Background(), run.RunID); err != nil {
			t.Error(err)
		}
	}()
	begin := time.Now()
	got := svc.executeWantedTarget(ctx, run, target)
	elapsed := time.Since(begin)
	if elapsed > 5*time.Second {
		t.Fatalf("the search was not interrupted by cancellation: took %s", elapsed)
	}
	slow.mu.Lock()
	waited := slow.waited
	slow.mu.Unlock()
	if waited > 5*time.Second {
		t.Fatalf("the indexer never saw the cancelled context: waited %s", waited)
	}
	if got.Grabbed != "" || got.Seen != 0 {
		t.Fatalf("an interrupted search reported results: %+v", got)
	}
	if got.State != "searched" && got.Skipped != "cancelled" {
		t.Fatalf("interrupted target = %+v", got)
	}
}

// ---- search planning: degraded, prohibited and movie capabilities ----

func TestGapAcqQueriesForIndexerHonoursDegradedProhibitedAndMovieCapabilities(t *testing.T) {
	ctx := context.Background()
	movie := domain.MovieWantable{Item: 1, Mon: true, Identity: domain.MediaIdentity{
		Title: "Gap Movie", Year: 2001, IDs: domain.ExternalIDs{IMDB: "tt0000001"},
	}}

	t.Run("degraded discovery warns and probes generically", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: ports.IndexerCapabilities{Degraded: true}}
		queries, warnings, err := queriesForIndexer(ctx, idx, movie, false)
		if err != nil || len(queries) != 1 || queries[0].Mode != "generic" {
			t.Fatalf("queries = %+v, %v", queries, err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "generic fallback") {
			t.Fatalf("warnings = %v", warnings)
		}
	})

	t.Run("prohibited movie mode falls back to generic titles", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: ports.IndexerCapabilities{
			Generic: ports.IndexerSearchCapability{Known: true, Available: true, Parameters: map[string]bool{"q": true}},
			Movie:   ports.IndexerSearchCapability{Known: true, Available: false, Parameters: map[string]bool{"q": true, "imdbid": true}},
		}}
		queries, _, err := queriesForIndexer(ctx, idx, movie, false)
		if err != nil || len(queries) != 1 || queries[0].Mode != "generic" || queries[0].ID != nil {
			t.Fatalf("queries = %+v, %v", queries, err)
		}
	})

	t.Run("movie mode with an imdb id uses movie queries", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: ports.IndexerCapabilities{
			Movie: ports.IndexerSearchCapability{Known: true, Available: true, Parameters: map[string]bool{"q": true, "imdbid": true}},
		}}
		queries, _, err := queriesForIndexer(ctx, idx, movie, false)
		if err != nil || len(queries) != 2 {
			t.Fatalf("queries = %+v, %v", queries, err)
		}
		if queries[0].Mode != "movie" || queries[0].ID == nil || queries[0].ID.Provider != "imdb" || queries[1].Mode != "movie" || queries[1].Q == "" {
			t.Fatalf("queries = %+v", queries)
		}
	})

	t.Run("no compatible mode is an unsupported query", func(t *testing.T) {
		idx := &gapCapsIndexer{caps: ports.IndexerCapabilities{
			Generic: ports.IndexerSearchCapability{Known: true, Available: true, Parameters: map[string]bool{}},
			Movie:   ports.IndexerSearchCapability{Known: true, Available: true, Parameters: map[string]bool{"tmdbid": true}},
		}}
		_, _, err := queriesForIndexer(ctx, idx, movie, true)
		var remote *ports.RemoteError
		if !errors.As(err, &remote) || remote.Category != ports.RemoteUnsupportedQuery {
			t.Fatalf("err = %v, want unsupported_query", err)
		}
	})
}

// ---- manual import queueing: every refusal before a row is written ----

func TestGapAcqQueueManualImportRefusesBadSelectionsBeforeWritingARow(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	bookID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindBook, Title: "Gap Book", SortTitle: "gap book", Author: "A. Writer",
		BookType: quality.BookTypeEbook, Monitored: true, Path: t.TempDir(),
		QualityProfileID: quality.EbookProfileID,
	})
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	withVideo := t.TempDir()
	if err := os.WriteFile(filepath.Join(withVideo, epRelease+".mkv"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  ManualImportRequest
		want string
	}{
		{"blank path", ManualImportRequest{Path: "  ", MediaItemID: itemID}, "required"},
		{"no item", ManualImportRequest{Path: withVideo}, "required"},
		{"unknown item", ManualImportRequest{Path: withVideo, MediaItemID: 999999}, sqlite.ErrNotFound.Error()},
		{"missing payload", ManualImportRequest{Path: filepath.Join(empty, "gone"), MediaItemID: itemID}, "payload missing"},
		{"no media in folder", ManualImportRequest{Path: empty, MediaItemID: itemID}, "no media files"},
		{"video is not a book", ManualImportRequest{Path: withVideo, MediaItemID: bookID}, "no media files"},
		{"empty selection", ManualImportRequest{Path: withVideo, Paths: []string{}, MediaItemID: itemID}, "no files selected"},
		{"selection outside payload", ManualImportRequest{Path: withVideo, Paths: []string{filepath.Join(empty, "x.mkv")}, MediaItemID: itemID}, "outside"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := svc.QueueManualImport(ctx, tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("QueueManualImport = %d, %v; want error mentioning %q", id, err, tc.want)
			}
			if id != 0 {
				t.Fatalf("a refused request returned row %d", id)
			}
		})
	}
	rows, err := db.ListRecentDownloads(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("refused requests wrote Activity rows: %+v, %v", rows, err)
	}
}

// ---- import event: a legacy book row without a medium ----

func TestGapAcqImportedEventOmitsBookIdentityWithoutAMedium(t *testing.T) {
	item := domain.MediaItem{ID: 9, Kind: domain.KindBook, Title: "Gap Book", Author: "A. Writer"}
	res := ImportResult{Imported: 1, Files: []FileOutcome{
		{Name: "a.mp3", Imported: true, Path: "/lib/Gap Book/a.mp3"},
		{Name: "b.mp3", Imported: false},
	}}
	event := importedEvent(item, sqlite.Download{ID: 3}, res)
	if event.BookTitle != "Gap Book" || event.BookAuthor != "A. Writer" {
		t.Fatalf("book text = %q by %q", event.BookTitle, event.BookAuthor)
	}
	if event.BookMedium != "" || event.BookWorkID != "" || event.BookEditionID != "" {
		t.Fatalf("a book with no known medium carried identity: %+v", event)
	}
	if len(event.Paths) != 1 || len(event.Dirs) != 1 || event.Dirs[0] != "/lib/Gap Book" {
		t.Fatalf("paths = %v dirs = %v", event.Paths, event.Dirs)
	}
}
