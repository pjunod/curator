package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// The replacement grab worker (contract section 3.3): per-kind grabs, the
// bytes budget, retries on the environment, and settling.

type erroringIndexer struct{ err error }

func (e erroringIndexer) Search(context.Context, domain.SearchQuery) ([]ports.Release, error) {
	return nil, e.err
}
func (erroringIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (erroringIndexer) Test(context.Context) error                        { return nil }

// failedWithGrab records a blamed failure for E01 and returns its row, with
// the grab written and not yet run.
func failedWithGrab(t *testing.T, f *v2Fixture) int64 {
	t.Helper()
	f.deferGrabs()
	dl := f.grab(1, epOne)
	f.observe(dl, terminalFact(dl, failed(ports.ReasonHealth, true), "event 1"))
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=? AND result=''`, dl.ID); n != 1 {
		t.Fatalf("replacement_grabs = %d, want one pending", n)
	}
	return dl.ID
}

func (f *v2Fixture) grabResult(id int64) (string, int, time.Time) {
	f.t.Helper()
	g, ok, err := f.db.GetReplacementGrab(f.ctx, id)
	if err != nil || !ok {
		f.t.Fatalf("grab %d: ok=%v err=%v", id, ok, err)
	}
	return g.Result, g.Attempts, g.NextAt
}

// The bytes budget: three releases' worth of blamed fetches in the window
// caps the want; the cap is recorded and nothing is grabbed.
func TestReplacementGrabRespectsTheBytesBudget(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	id := failedWithGrab(t, f)
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO downloads(media_item_id,protocol,release_title,state,client_id,wantables,size,progress,
			runner_outcome,runner_blames_release,added_at,updated_at) VALUES(?,'torrent',?,'failed',1,'["episode:1:1:1"]',1000,1,'failed',1,?,?)`,
			f.item, "earlier-"+string(rune('a'+i)), now, now); err != nil {
			t.Fatal(err)
		}
	}
	f.svc.RunReplacementGrabs(f.ctx)
	result, _, _ := f.grabResult(id)
	if result != "nothing to grab" || f.runner.addCount() != 1 {
		t.Fatalf("result %q adds %d, want the capped want left alone", result, f.runner.addCount())
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type=?`, HistoryRegrabCapped); n != 1 {
		t.Fatalf("regrab_capped history = %d, want the cap recorded", n)
	}
}

// Failures that cost nothing do not spend the budget: a local fault fetched
// in full is not the release's.
func TestBlamelessFailuresDoNotSpendTheBudget(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	id := failedWithGrab(t, f)
	now := time.Now().UnixMilli()
	for i := 0; i < 3; i++ {
		if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO downloads(media_item_id,protocol,release_title,state,client_id,wantables,size,progress,
			runner_outcome,runner_blames_release,added_at,updated_at) VALUES(?,'torrent',?,'failed',1,'["episode:1:1:1"]',1000,1,'failed',0,?,?)`,
			f.item, "io-"+string(rune('a'+i)), now, now); err != nil {
			t.Fatal(err)
		}
	}
	f.svc.RunReplacementGrabs(f.ctx)
	if result, _, _ := f.grabResult(id); !strings.HasPrefix(result, "grabbed") || f.runner.addCount() != 2 {
		t.Fatalf("result %q adds %d, want the replacement grabbed", result, f.runner.addCount())
	}
}

// An environment failure re-queues the grab until its attempts are spent;
// then the want is left to the wanted pass, deferred.
func TestReplacementGrabRetriesOnTheEnvironment(t *testing.T) {
	t.Run("no enabled indexer", func(t *testing.T) {
		f := setupV2(t, nil)
		id := failedWithGrab(t, f)
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE indexers SET enabled=0`); err != nil {
			t.Fatal(err)
		}
		f.svc.RunReplacementGrabs(f.ctx)
		result, attempts, next := f.grabResult(id)
		if result != "" || attempts != 1 || !next.After(time.Now()) {
			t.Fatalf("grab = %q attempts %d next %s, want re-queued", result, attempts, next)
		}
		for i := 0; i < 3; i++ {
			if _, err := f.db.W.ExecContext(f.ctx, `UPDATE replacement_grabs SET next_at=0 WHERE download_id=?`, id); err != nil {
				t.Fatal(err)
			}
			f.svc.RunReplacementGrabs(f.ctx)
		}
		if result, _, _ := f.grabResult(id); result != "deferred: no enabled indexer" {
			t.Fatalf("grab = %q, want deferred after its attempts", result)
		}
	})
	t.Run("an unreachable client", func(t *testing.T) {
		f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
		id := failedWithGrab(t, f)
		f.runner.mu.Lock()
		f.runner.addErr = errors.New("connection refused")
		f.runner.mu.Unlock()
		f.svc.RunReplacementGrabs(f.ctx)
		result, attempts, next := f.grabResult(id)
		if result != "" || attempts != 1 || !next.After(time.Now().Add(30*time.Second)) {
			t.Fatalf("grab = %q attempts %d next %s, want re-queued a minute out", result, attempts, next)
		}
		f.runner.mu.Lock()
		f.runner.addErr = nil
		f.runner.mu.Unlock()
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE replacement_grabs SET next_at=0 WHERE download_id=?`, id); err != nil {
			t.Fatal(err)
		}
		f.svc.RunReplacementGrabs(f.ctx)
		// The refused Add left its own row submitting, which reconciliation
		// owns: the retry does not grab the want a second time.
		if result, _, _ := f.grabResult(id); result != "nothing to grab" {
			t.Fatalf("grab after the client is back = %q, want settled", result)
		}
		if n := f.count(`SELECT COUNT(*) FROM downloads WHERE state='submitting'`); n != 1 {
			t.Fatalf("submitting rows = %d, want the one uncertain submission and no duplicate", n)
		}
	})
	// Every indexer failing is not an environment retry here: the want stays
	// wanted for the wanted pass, and it is not handed to the season planner
	// as if no single release existed.
	t.Run("every indexer failing leaves the want to the wanted pass", func(t *testing.T) {
		f := setupV2(t, nil)
		id := failedWithGrab(t, f)
		f.svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return erroringIndexer{errors.New("indexer 503")} }
		f.svc.RunReplacementGrabs(f.ctx)
		result, _, _ := f.grabResult(id)
		if result != "nothing to grab" || f.runner.addCount() != 1 {
			t.Fatalf("grab = %q adds %d, want settled without a grab", result, f.runner.addCount())
		}
		if n := f.count(`SELECT COUNT(*) FROM jobs WHERE kind=?`, SeasonSearchJobKind); n != 0 {
			t.Fatalf("season planner jobs = %d, want none for an incomplete search", n)
		}
		if w, err := f.svc.wantableFromID(f.ctx, "episode:"+itoa(f.item)+":1:1"); err != nil || !f.svc.stillWanted(f.ctx, w) {
			t.Fatalf("the want is not wanted any more (%v)", err)
		}
	})
}

// Settling without a grab: a row that is gone, a want that is not wanted any
// more, and a wantable id this library cannot read.
func TestReplacementGrabSettlesWithoutAGrab(t *testing.T) {
	t.Run("a want no longer wanted", func(t *testing.T) {
		f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
		id := failedWithGrab(t, f)
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE episodes SET monitored=0`); err != nil {
			t.Fatal(err)
		}
		f.svc.RunReplacementGrabs(f.ctx)
		if result, _, _ := f.grabResult(id); result != "nothing to grab" || f.runner.addCount() != 1 {
			t.Fatalf("result %q adds %d, want nothing grabbed for an unmonitored episode", result, f.runner.addCount())
		}
	})
	t.Run("an unreadable wantable", func(t *testing.T) {
		f := setupV2(t, nil)
		id := failedWithGrab(t, f)
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE replacement_grabs SET wantable_ids='["nonsense"]' WHERE download_id=?`, id); err != nil {
			t.Fatal(err)
		}
		f.svc.RunReplacementGrabs(f.ctx)
		if result, _, _ := f.grabResult(id); result != "nothing to grab" {
			t.Fatalf("result %q, want settled with nothing to grab", result)
		}
	})
	t.Run("a cancelled run claims nothing", func(t *testing.T) {
		f := setupV2(t, nil)
		id := failedWithGrab(t, f)
		ctx, cancel := context.WithCancel(f.ctx)
		cancel()
		f.svc.RunReplacementGrabs(ctx)
		if result, attempts, _ := f.grabResult(id); result != "" || attempts != 0 {
			t.Fatalf("grab = %q attempts %d, want untouched", result, attempts)
		}
	})
	t.Run("a season pack's grab expands to its wanted episodes", func(t *testing.T) {
		f := setupV2(t, []ports.Release{rel("Test.Show.S01E02.1080p.WEB-DL.x264-SINGLE", 10)})
		f.deferGrabs()
		pack := f.grab(0, seasonPack)
		f.observe(pack, terminalFact(pack, failed(ports.ReasonHealth, true), "event 1"))
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE replacement_grabs SET wantable_ids='["season:`+itoa(f.item)+`:1"]' WHERE download_id=?`, pack.ID); err != nil {
			t.Fatal(err)
		}
		f.svc.RunReplacementGrabs(f.ctx)
		result, _, _ := f.grabResult(pack.ID)
		if !strings.Contains(result, "episode:"+itoa(f.item)+":1:2") {
			t.Fatalf("result %q, want E02 grabbed from the season's expansion", result)
		}
	})
}
