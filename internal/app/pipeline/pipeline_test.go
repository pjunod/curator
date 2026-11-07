package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func fixture(t *testing.T) (*Timings, *sqlite.DB, *time.Time) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	p := New(db)
	p.now = func() time.Time { return clock }
	return p, db, &clock
}

func addDownload(t *testing.T, db *sqlite.DB, title, state, protocol string) int64 {
	t.Helper()
	ctx := context.Background()
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: title, Path: "/library/" + title})
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: state, Protocol: protocol, ReleaseTitle: title + ".1080p"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestKeyFallsBackToDownloadID(t *testing.T) {
	if Key("t-1-abc", 9) != "t-1-abc" || Key("", 9) != "download-9" {
		t.Fatal("Key must prefer the transfer token and fall back to download-<id>")
	}
}

func TestStartEndAndPercentiles(t *testing.T) {
	p, _, clock := fixture(t)
	ctx := context.Background()
	base := *clock
	// Ten finished timings of 1..10 seconds, one still waiting.
	for i := 1; i <= 10; i++ {
		key := Key("", int64(i))
		*clock = base
		p.Start(ctx, FailureToNextGrab, key, int64(i))
		*clock = base.Add(time.Duration(i) * time.Second)
		p.End(ctx, FailureToNextGrab, key)
	}
	*clock = base
	p.Start(ctx, FailureToNextGrab, "still-waiting", 99)
	// A second start must not move the clock; a second end must not either.
	*clock = base.Add(time.Hour)
	p.Start(ctx, FailureToNextGrab, "still-waiting", 99)
	*clock = base.Add(time.Second)
	p.End(ctx, FailureToNextGrab, Key("", 1))

	*clock = base.Add(time.Minute)
	p50, p95, waiting := p.Percentiles(ctx, FailureToNextGrab, Window)
	if p50 != 5*time.Second || p95 != 10*time.Second || waiting != 1 {
		t.Fatalf("p50=%s p95=%s waiting=%d, want 5s 10s 1", p50, p95, waiting)
	}
	// Ending a timing that was never started changes nothing.
	p.End(ctx, RunnerDoneToImport, "never-started")
	if s, _ := p.Stats(ctx, RunnerDoneToImport, Window); s.Samples != 0 || s.Waiting != 0 {
		t.Fatalf("stats for unstarted kind = %+v", s)
	}
}

func TestNilTimingsIsInert(t *testing.T) {
	var p *Timings
	ctx := context.Background()
	p.Start(ctx, FailureToNextGrab, "x", 1)
	p.End(ctx, FailureToNextGrab, "x")
	if rows, err := p.Overdue(ctx, 10); err != nil || rows != nil {
		t.Fatalf("nil Overdue = %v %v", rows, err)
	}
	if r := p.OverdueCheck(ctx); r.Status != health.StatusOK {
		t.Fatalf("nil OverdueCheck = %+v", r)
	}
	var buf bytes.Buffer
	p.WriteMetrics(ctx, &buf)
	if !strings.Contains(buf.String(), "monarr_failure_to_next_grab_seconds_count 0") {
		t.Fatalf("nil metrics must still list the families:\n%s", buf.String())
	}
}

func TestOverdueCheckNamesTheStaleRow(t *testing.T) {
	p, db, clock := fixture(t)
	ctx := context.Background()
	if r := p.OverdueCheck(ctx); r.Status != health.StatusOK {
		t.Fatalf("empty queue = %+v, want ok", r)
	}
	stale := addDownload(t, db, "Stale.Movie", "grabbed", "usenet")
	fresh := addDownload(t, db, "Fresh.Movie", "downloading", "usenet")
	torrent := addDownload(t, db, "Slow.Torrent", "downloading", "torrent")
	waiting := addDownload(t, db, "Waiting.On.Person", "awaiting_import", "usenet")
	done := addDownload(t, db, "Done.Movie", "imported", "usenet")
	_, _, _, _ = fresh, torrent, waiting, done

	// 20 minutes on: the grabbed row (allowance 10m) is overdue; the 30 m
	// usenet row, the 24 h torrent row, the operator-gated row and the
	// terminal row are not.
	*clock = time.Now().Add(20 * time.Minute)
	r := p.OverdueCheck(ctx)
	if r.Status != health.StatusWarning {
		t.Fatalf("status = %s (%s), want warning", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "Stale.Movie.1080p") || !strings.Contains(r.Message, "#"+strconv.FormatInt(stale, 10)) {
		t.Errorf("message does not name the stale row: %s", r.Message)
	}
	for _, other := range []string{"Fresh.Movie", "Slow.Torrent", "Waiting.On.Person", "Done.Movie"} {
		if strings.Contains(r.Message, other) {
			t.Errorf("message names %s, which is within its allowance: %s", other, r.Message)
		}
	}
	// The message must say what the check measures (silence from the client)
	// and what it does not catch (jobs held inside the client).
	if !strings.Contains(r.Message, "not heard") || !strings.Contains(r.Message, "held") {
		t.Errorf("message must say it judges silence from the client and does not catch held jobs: %s", r.Message)
	}
	if !strings.Contains(r.Message, "M2") {
		t.Errorf("message must say the deadline column is not there yet: %s", r.Message)
	}

	// 40 minutes on: the usenet downloading row joins; the torrent does not.
	*clock = time.Now().Add(40 * time.Minute)
	rows, err := p.Overdue(ctx, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("overdue = %+v err %v, want the grabbed and the usenet downloading rows", rows, err)
	}
}

func TestWriteMetricsListsHistogramFamilies(t *testing.T) {
	p, _, clock := fixture(t)
	ctx := context.Background()
	base := *clock
	p.Start(ctx, RunnerDoneToImport, "t-1", 1)
	*clock = base.Add(45 * time.Second)
	p.End(ctx, RunnerDoneToImport, "t-1")

	var buf bytes.Buffer
	p.WriteMetrics(ctx, &buf)
	out := buf.String()
	for _, want := range []string{
		"# TYPE monarr_failure_to_next_grab_seconds histogram",
		"# TYPE monarr_runner_done_to_import_seconds histogram",
		"# TYPE monarr_import_to_cinema_visible_seconds histogram",
		`monarr_runner_done_to_import_seconds_bucket{le="30"} 0`,
		`monarr_runner_done_to_import_seconds_bucket{le="60"} 1`,
		`monarr_runner_done_to_import_seconds_bucket{le="+Inf"} 1`,
		"monarr_runner_done_to_import_seconds_sum 45",
		"monarr_runner_done_to_import_seconds_count 1",
		`monarr_pipeline_waiting{kind="import_to_cinema_visible"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q:\n%s", want, out)
		}
	}
}

func TestParkedAndSupersededRowsAreNeverOverdue(t *testing.T) {
	p, db, clock := fixture(t)
	ctx := context.Background()
	parked := addDownload(t, db, "Parked.Movie", "grabbed", "usenet")
	addDownload(t, db, "Open.Movie", "grabbed", "usenet")
	// ParkDownload stamps parked_at but leaves updated_at alone.
	if err := db.ParkDownload(ctx, parked, "operator cancellation"); err != nil {
		t.Fatal(err)
	}
	*clock = time.Now().Add(time.Hour)
	r := p.OverdueCheck(ctx)
	if r.Status != health.StatusWarning || strings.Contains(r.Message, "Parked.Movie") || !strings.Contains(r.Message, "Open.Movie") {
		t.Fatalf("check = %+v, want only the unparked row named", r)
	}
	if n, err := p.OverdueCount(ctx); err != nil || n != 1 {
		t.Fatalf("count = %d err %v, want 1", n, err)
	}
}

func TestDiscardDropsOnlyOpenTimings(t *testing.T) {
	p, _, clock := fixture(t)
	ctx := context.Background()
	base := *clock
	p.Start(ctx, FailureToNextGrab, "open", 1)
	p.Start(ctx, FailureToNextGrab, "closed", 2)
	*clock = base.Add(time.Second)
	p.End(ctx, FailureToNextGrab, "closed")
	p.Discard(ctx, FailureToNextGrab, "open")
	p.Discard(ctx, FailureToNextGrab, "closed")
	s, _ := p.Stats(ctx, FailureToNextGrab, Window)
	if s.Samples != 1 || s.Waiting != 0 {
		t.Fatalf("stats = %+v, want the closed timing kept and the open one gone", s)
	}
}

func TestEndBeforeStartClosesWithZeroDuration(t *testing.T) {
	p, _, clock := fixture(t)
	ctx := context.Background()
	base := *clock
	p.Start(ctx, RunnerDoneToImport, "t-clock", 1)
	// The wall clock stepped backwards between start and end: the sample must
	// still close (duration 0) rather than stay open and count as waiting.
	*clock = base.Add(-5 * time.Second)
	p.End(ctx, RunnerDoneToImport, "t-clock")
	*clock = base.Add(time.Minute)
	s, err := p.Stats(ctx, RunnerDoneToImport, Window)
	if err != nil {
		t.Fatal(err)
	}
	if s.Samples != 1 || s.Waiting != 0 {
		t.Fatalf("stats = %+v, want 1 closed sample and none waiting", s)
	}
}

func TestWriteMetricsOmitsSeriesWhenTheQueryFails(t *testing.T) {
	p, db, _ := fixture(t)
	ctx := context.Background()
	// A failed query must not be written as zeros (Prometheus would read a
	// counter reset); the family is left out and the error counted.
	db.Close()
	var buf bytes.Buffer
	p.WriteMetrics(ctx, &buf)
	out := buf.String()
	if strings.Contains(out, "_bucket{") || strings.Contains(out, "monarr_pipeline_waiting{") {
		t.Fatalf("series written despite failed queries:\n%s", out)
	}
	want := fmt.Sprintf("monarr_pipeline_scrape_errors_total %d", 2*len(Kinds))
	if !strings.Contains(out, want) {
		t.Fatalf("metrics missing %q:\n%s", want, out)
	}
}
