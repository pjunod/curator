package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// These tests pin one rule: who asked decides which request allowance a
// search is charged to, wherever the search ends up running. The automatic
// allowance is rationed so unattended passes cannot starve each other; a
// person pressing a button was being rationed with them, so the button did
// nothing for hours after every backlog pass.

// spendAllowance debits every indexer in one bucket until the budget refuses.
func spendAllowance(t *testing.T, db *sqlite.DB, bucket string, automatic bool) {
	t.Helper()
	ctx := context.Background()
	indexers, err := db.ListIndexers(ctx)
	if err != nil || len(indexers) == 0 {
		t.Fatalf("indexers = %v, err = %v", indexers, err)
	}
	for _, cfg := range indexers {
		var deferred *sqlite.BudgetDeferred
		for i := 0; ; i++ {
			err := db.ReserveIndexerRequest(ctx, cfg.ID, bucket, automatic, time.Now())
			if errors.As(err, &deferred) {
				break
			}
			if err != nil || i > 1000 {
				t.Fatalf("spending %s allowance: i=%d err=%v", bucket, i, err)
			}
		}
	}
}

func usageRows(t *testing.T, db *sqlite.DB) (automatic, manual int) {
	t.Helper()
	if err := db.R.QueryRowContext(context.Background(), `SELECT count(*) FILTER (WHERE automatic=1), count(*) FILTER (WHERE automatic=0) FROM indexer_request_usage`).Scan(&automatic, &manual); err != nil {
		t.Fatal(err)
	}
	return automatic, manual
}

func wantedFixtureTarget(t *testing.T, svc *Service, db *sqlite.DB, req WantedSearchRequest) (sqlite.WantedSearchRun, sqlite.WantedSearchTarget) {
	t.Helper()
	ctx := context.Background()
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	delay := 0
	req.TargetDelay = &delay
	started, err := svc.StartWantedSearch(ctx, req)
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
	return run, target
}

// A Wanted run aimed at one title is a person waiting on it. With the
// automatic share spent it must still search, and nothing it sends may be
// booked as automatic.
func TestWantedRunForOneTitleIsChargedAsManual(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 50)}, client)
	run, target := wantedFixtureTarget(t, svc, db, WantedSearchRequest{Scope: "group", MediaItemID: movieID})
	spendAllowance(t, db, "search", true)
	before, _ := usageRows(t, db)

	got := svc.executeWantedTarget(context.Background(), run, target)
	if got.State != "searched" || got.Grabbed == "" || got.Error != "" {
		t.Fatalf("result = %+v, want a grab despite the spent automatic share", got)
	}
	after, manual := usageRows(t, db)
	if after != before || manual == 0 {
		t.Fatalf("automatic rows %d -> %d, manual %d; want the run charged as manual only", before, after, manual)
	}
}

// "Search everything" is the backlog on demand. It stays unattended work, so
// it cannot spend the reserve kept for deliberate searches — and when the
// automatic share is gone its targets say so instead of "searched, 0 seen",
// the lie that made a spent allowance look like an empty indexer.
func TestWantedRunForEverythingStaysAutomaticAndSaysWhenRefused(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 50)}, client)
	run, target := wantedFixtureTarget(t, svc, db, WantedSearchRequest{Scope: "all"})
	spendAllowance(t, db, "search", true)
	_, manualBefore := usageRows(t, db)

	got := svc.executeWantedTarget(context.Background(), run, target)
	if got.State != "skipped" || got.Skipped != SkipIndexersUnavailable {
		t.Fatalf("result = %+v, want skipped as %q", got, SkipIndexersUnavailable)
	}
	if !strings.Contains(got.Error, "request allowance exhausted") || got.Seen != 0 || len(client.added) != 0 {
		t.Fatalf("result = %+v grabs=%v, want the refusal named and nothing grabbed", got, client.added)
	}
	if _, manual := usageRows(t, db); manual != manualBefore {
		t.Fatalf("manual rows %d -> %d; a bulk run must not spend the manual allowance", manualBefore, manual)
	}
}

func seriesFixture(t *testing.T) (*Service, *sqlite.DB, int64, *recordingIndexer) {
	t.Helper()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	idx := &recordingIndexer{}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return idx }
	if _, err := db.W.ExecContext(context.Background(), `UPDATE episodes SET air_date = '2020-01-01' WHERE media_item_id = ? AND season_number > 0`, itemID); err != nil {
		t.Fatal(err)
	}
	return svc, db, itemID, idx
}

func seasonJobManual(t *testing.T, db *sqlite.DB) (count int, manual bool) {
	t.Helper()
	rows, err := db.R.QueryContext(context.Background(), `SELECT COALESCE(json_extract(payload,'$.manual'),0) FROM jobs WHERE kind=? AND state='queued'`, SeasonSearchJobKind)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	manual = true
	for rows.Next() {
		var m bool
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		count++
		manual = manual && m
	}
	return count, count > 0 && manual
}

// The series button queues a season comparison that runs later on a worker.
// The request context is long gone by then, so "a person asked" has to ride
// in the job payload, and the comparison has to spend the manual allowance.
func TestSeriesAutoSearchCarriesManualAttributionIntoTheJob(t *testing.T) {
	ctx := context.Background()

	t.Run("an unattended season comparison waits for the automatic share", func(t *testing.T) {
		svc, db, itemID, idx := seriesFixture(t)
		if _, err := svc.AutoSearchItem(ctx, itemID); err != nil {
			t.Fatal(err)
		}
		if n, manual := seasonJobManual(t, db); n == 0 || manual {
			t.Fatalf("queued season jobs = %d manual = %v, want unattended jobs", n, manual)
		}
		spendAllowance(t, db, "search", true)
		job, err := db.ClaimJob(ctx, "season-fixture", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		var deferred *sqlite.BudgetDeferred
		if err := svc.handleSeasonSearch(ctx, job); !errors.As(err, &deferred) {
			t.Fatalf("err = %v, want the comparison deferred by the automatic share", err)
		}
		if asked := idx.asked(); len(asked) != 0 {
			t.Fatalf("asked = %v, want no query sent", asked)
		}
	})

	t.Run("the button's comparison runs on the manual allowance", func(t *testing.T) {
		svc, db, itemID, idx := seriesFixture(t)
		spendAllowance(t, db, "search", true)
		before, _ := usageRows(t, db)
		out, err := svc.AutoSearchItem(WithManualSearch(ctx), itemID)
		if err != nil || len(out.Targets) == 0 {
			t.Fatalf("outcome = %+v err = %v", out, err)
		}
		if n, manual := seasonJobManual(t, db); n == 0 || !manual {
			t.Fatalf("queued season jobs = %d manual = %v, want manual jobs", n, manual)
		}
		runQueuedSeason(t, svc, db)
		if asked := idx.asked(); len(asked) == 0 {
			t.Fatal("no query was sent; the manual comparison was rationed as unattended work")
		}
		after, manual := usageRows(t, db)
		if after != before || manual == 0 {
			t.Fatalf("automatic rows %d -> %d, manual %d; want the comparison charged as manual only", before, after, manual)
		}
	})

	t.Run("pressing the button promotes a comparison already parked", func(t *testing.T) {
		svc, db, itemID, _ := seriesFixture(t)
		if _, err := svc.AutoSearchItem(ctx, itemID); err != nil {
			t.Fatal(err)
		}
		later := time.Now().Add(6 * time.Hour).UnixMilli()
		if _, err := db.W.ExecContext(ctx, `UPDATE jobs SET run_after=? WHERE kind=?`, later, SeasonSearchJobKind); err != nil {
			t.Fatal(err)
		}
		queued, _ := seasonJobManual(t, db)
		if _, err := svc.AutoSearchItem(WithManualSearch(ctx), itemID); err != nil {
			t.Fatal(err)
		}
		n, manual := seasonJobManual(t, db)
		if n != queued || !manual {
			t.Fatalf("queued season jobs %d -> %d manual = %v, want the same jobs promoted", queued, n, manual)
		}
		var parked int
		if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=? AND state='queued' AND run_after>?`, SeasonSearchJobKind, time.Now().Add(time.Minute).UnixMilli()).Scan(&parked); err != nil {
			t.Fatal(err)
		}
		if parked != 0 {
			t.Fatalf("%d promoted jobs are still parked in the future", parked)
		}
	})

	t.Run("the week-long cooldown does not silence the button", func(t *testing.T) {
		svc, db, itemID, _ := seriesFixture(t)
		if _, err := svc.AutoSearchItem(ctx, itemID); err != nil {
			t.Fatal(err)
		}
		first, _ := seasonJobManual(t, db)
		for range first {
			runQueuedSeason(t, svc, db)
		}
		// Unattended, inside the cooldown: nothing is queued, and the answer
		// says so rather than "queued".
		out, err := svc.AutoSearchItem(ctx, itemID)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := seasonJobManual(t, db); n != 0 || len(out.Targets) != first || out.Targets[0].Skipped != SkipComparedRecently {
			t.Fatalf("unattended request inside the cooldown: queued %d, outcome %+v", n, out)
		}
		// A person asking is not bound by the unattended cooldown.
		out, err = svc.AutoSearchItem(WithManualSearch(ctx), itemID)
		if err != nil {
			t.Fatal(err)
		}
		if n, manual := seasonJobManual(t, db); n != first || !manual || out.Targets[0].Skipped != "queued" {
			t.Fatalf("manual request queued %d jobs (manual=%v), outcome %+v, want %d queued", n, manual, out, first)
		}
		for range first {
			runQueuedSeason(t, svc, db)
		}
		// Their own comparison finished seconds ago: a second press does not
		// spend the allowance again, and is told why.
		out, err = svc.AutoSearchItem(WithManualSearch(ctx), itemID)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := seasonJobManual(t, db); n != 0 || out.Targets[0].Skipped != SkipComparedRecently {
			t.Fatalf("repeat press right after a manual comparison: queued %d, outcome %+v", n, out)
		}
		// Ten minutes on, the button works again.
		if _, err := db.W.ExecContext(ctx, `UPDATE jobs SET payload=json_set(payload,'$.cooldownUntil',?) WHERE kind=? AND state='done'`, time.Now().Add(seasonCooldown-manualSeasonCooldown-time.Minute).UTC().Format(time.RFC3339Nano), SeasonSearchJobKind); err != nil {
			t.Fatal(err)
		}
		if _, err = svc.AutoSearchItem(WithManualSearch(ctx), itemID); err != nil {
			t.Fatal(err)
		}
		if n, _ := seasonJobManual(t, db); n != first {
			t.Fatalf("after the manual cooldown: queued %d, want %d", n, first)
		}
	})

	t.Run("a manual comparison does not wait behind one parked on the automatic share", func(t *testing.T) {
		svc, db, itemID, idx := seriesFixture(t)
		// Another season's unattended comparison was admitted and then
		// parked for hours on the spent automatic share.
		parked, _ := json.Marshal(seasonCheckpoint{Version: 1, ItemID: 424242, Season: 1, Trigger: "backlog", Started: time.Now()})
		if _, err := db.EnqueueJob(ctx, domain.Job{Kind: SeasonSearchJobKind, Payload: string(parked), DedupeKey: seasonJobKey(424242, 0, 1), Priority: 110, MaxAttempts: 3, RunAfter: time.Now().Add(6 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		spendAllowance(t, db, "search", true)
		if _, err := svc.AutoSearchItem(WithManualSearch(ctx), itemID); err != nil {
			t.Fatal(err)
		}
		runQueuedSeason(t, svc, db)
		if asked := idx.asked(); len(asked) == 0 {
			t.Fatal("the manual comparison sent nothing; it waited behind the parked unattended one")
		}
	})

	t.Run("a job promoted while a worker holds it reruns at once as manual", func(t *testing.T) {
		svc, db, itemID, idx := seriesFixture(t)
		if _, err := svc.AutoSearchItem(ctx, itemID); err != nil {
			t.Fatal(err)
		}
		spendAllowance(t, db, "search", true)
		job, err := db.ClaimJob(ctx, "season-fixture", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		// The press lands while the worker holds the job with its old payload.
		if _, err := svc.AutoSearchItem(WithManualSearch(ctx), itemID); err != nil {
			t.Fatal(err)
		}
		var deferred *sqlite.BudgetDeferred
		err = svc.handleSeasonSearch(ctx, job)
		if !errors.As(err, &deferred) || time.Until(deferred.At) > time.Second {
			t.Fatalf("err = %v, want an immediate rerun instead of a park on the automatic share", err)
		}
		var manual bool
		if err := db.R.QueryRowContext(ctx, `SELECT COALESCE(json_extract(payload,'$.manual'),0) FROM jobs WHERE id=?`, job.ID).Scan(&manual); err != nil || !manual {
			t.Fatalf("stored manual = %v err = %v, want the promotion kept through the worker's checkpoint", manual, err)
		}
		if asked := idx.asked(); len(asked) != 0 {
			t.Fatalf("asked = %v, want nothing sent by the unattended step", asked)
		}
	})

	t.Run("the reruns a manual comparison schedules for itself are unattended", func(t *testing.T) {
		p := seasonCheckpoint{Manual: true}
		p.endManualEpoch()
		raw, _ := json.Marshal(p)
		if p.Manual || strings.Contains(string(raw), "manual") {
			t.Fatalf("checkpoint after the epoch ended = %s, want manual cleared", raw)
		}
	})
}

// The admission forecast has to simulate the pool the comparison will really
// spend. Forecast against the automatic share, a manual comparison is held
// back while the manual pool is free, and waved through when it is empty.
func TestManualSeasonForecastUsesTheManualPool(t *testing.T) {
	now := time.Now()
	b := sqlite.RequestBudget(250) // 20 interactive, 34 headroom, 100 search
	deadline := now.Add(23 * time.Hour)

	// The automatic share is fully spent for the next eleven hours...
	var day, half []time.Time
	for i := 0; i < b.Search; i++ {
		day = append(day, now.Add(23*time.Hour+30*time.Minute))
		half = append(half, now.Add(11*time.Hour))
	}
	if !feasibleFinish(now, b, day, half, 51, time.Second).After(deadline) {
		t.Fatal("fixture: the automatic forecast should not fit 51 requests")
	}
	// ...but the manual pool still has the reserve and headroom: 54 requests.
	if feasibleFinishManual(now, b, day, 54, time.Second).After(deadline) {
		t.Fatal("a manual comparison that fits the reserve and headroom was forecast as infeasible")
	}
	if !feasibleFinishManual(now, b, day, 55, time.Second).After(deadline) {
		t.Fatal("a manual comparison larger than the free manual pool was forecast as feasible")
	}
}
