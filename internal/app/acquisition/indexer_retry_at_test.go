package acquisition

import (
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestIndexerOutageUntilHonoursTheIndexersOwnTime(t *testing.T) {
	now := time.Unix(1_795_000_000, 0)
	for _, c := range []struct {
		name    string
		retryAt time.Time
		want    time.Time
	}{
		{"none named", time.Time{}, now.Add(indexerOutageRetry)},
		{"sooner than the floor", now.Add(time.Minute), now.Add(indexerOutageRetry)},
		{"quota reset", now.Add(23*time.Hour + 42*time.Minute), now.Add(23*time.Hour + 42*time.Minute)},
		{"beyond the ceiling", now.Add(30 * 24 * time.Hour), now.Add(indexerOutageCeiling)},
	} {
		if got := indexerOutageUntil(now, c.retryAt); !got.Equal(c.want) {
			t.Errorf("%s: until = %v, want %v", c.name, got, c.want)
		}
	}
}

// Good Trouble S02E06, 2026-11-25: nzb.life refused with Retry-After 85296 s.
// The terminal's retry_at defers the indexer (and the row) until then, not
// for the 15-minute default that let the next search hit the same refusal.
func TestIndexerRetryAtDefersTheIndexerUntilItsQuotaResets(t *testing.T) {
	f := setupV2(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-OTHER", 10)})
	dl := f.grab(1, epOne)
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET source_indexer_id=(SELECT MIN(id) FROM indexers) WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	dl = f.row(dl.ID)
	tm := failed(ports.ReasonFetch, false)
	tm.FetchCause = ports.FetchCauseIndexerUnavailable
	reset := time.Now().Add(85296 * time.Second).Truncate(time.Second)
	tm.RetryAt = reset
	f.observe(dl, terminalFact(dl, tm, "event 1"))
	if n := f.count(`SELECT COUNT(*) FROM downloads d JOIN indexers i ON i.id=d.source_indexer_id
		WHERE d.id=? AND d.provider_retry_after=? AND i.retry_at=?`, dl.ID, reset.UnixMilli(), reset.UnixMilli()); n != 1 {
		t.Fatal("the indexer and the row are not deferred until the indexer's own retry_at")
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs`); n != 0 || len(mustBlocklist(t, f)) != 0 {
		t.Fatalf("grabs=%d blocklist=%v, want neither", n, mustBlocklist(t, f))
	}
}
