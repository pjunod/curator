// Package pipeline measures the seams between Runner, Curator and Cinema
// (pipeline contract v2, docs/PIPELINE_CONTRACT.md section 3.5): how long a
// failure takes to become the next grab, how long a finished payload takes to
// become an import, and how long an import takes to become visible in Cinema.
// It also owns the overdue check that says whether anything is stuck.
//
// It observes; it never decides. Nothing in here retries, parks, or acts on a
// row.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Kinds of timing. The strings are the pipeline_timings.kind values and the
// stems of the Prometheus metric names.
const (
	FailureToNextGrab     = sqlite.TimingFailureToNextGrab
	RunnerDoneToImport    = sqlite.TimingRunnerDoneToImport
	ImportToCinemaVisible = sqlite.TimingImportToCinemaVisible
)

// Kinds lists the three timings in display order.
var Kinds = []string{FailureToNextGrab, RunnerDoneToImport, ImportToCinemaVisible}

// Window is the span the System tiles and the waiting counts look back over.
const Window = 24 * time.Hour

// Timings records and reads pipeline_timings. A nil *Timings is valid and
// records nothing, so tests and tools that build a Service without it work.
type Timings struct {
	db           *sqlite.DB
	log          *slog.Logger
	now          func() time.Time
	scrapeErrors atomic.Int64
}

// New returns a Timings over db.
func New(db *sqlite.DB) *Timings {
	return &Timings{db: db, log: slog.Default(), now: time.Now}
}

// Key is the transfer string a row's timings are filed under: its transfer
// token, or "download-<id>" for a row that never had one.
func Key(transfer string, downloadID int64) string {
	if transfer != "" {
		return transfer
	}
	return fmt.Sprintf("download-%d", downloadID)
}

// Start opens a timing. The first start for (kind, transfer) wins.
func (t *Timings) Start(ctx context.Context, kind, transfer string, downloadID int64) {
	if t == nil || t.db == nil {
		return
	}
	if err := t.db.StartPipelineTiming(ctx, kind, transfer, downloadID, t.now().UnixMilli()); err != nil {
		t.log.Warn("pipeline timing: start failed", "kind", kind, "transfer", transfer, "err", err)
	}
}

// Discard drops an open timing whose start turned out to have nothing to wait
// for, so it is not reported as waiting. A closed timing is left alone.
func (t *Timings) Discard(ctx context.Context, kind, transfer string) {
	if t == nil || t.db == nil {
		return
	}
	if err := t.db.DiscardPipelineTiming(ctx, kind, transfer); err != nil {
		t.log.Warn("pipeline timing: discard failed", "kind", kind, "transfer", transfer, "err", err)
	}
}

// End closes an open timing; a timing that was never started is ignored.
func (t *Timings) End(ctx context.Context, kind, transfer string) {
	if t == nil || t.db == nil {
		return
	}
	if err := t.db.EndPipelineTiming(ctx, kind, transfer, t.now().UnixMilli()); err != nil {
		t.log.Warn("pipeline timing: end failed", "kind", kind, "transfer", transfer, "err", err)
	}
}

// KindStats is one timing's last-window picture.
type KindStats struct {
	Kind    string
	Samples int           // closed timings in the window
	P50     time.Duration // 0 when Samples == 0
	P95     time.Duration
	Waiting int // open timings started in the window; never folded into the percentiles
}

// Stats reads one kind over window.
func (t *Timings) Stats(ctx context.Context, kind string, window time.Duration) (KindStats, error) {
	s := KindStats{Kind: kind}
	if t == nil || t.db == nil {
		return s, nil
	}
	since := t.now().Add(-window).UnixMilli()
	ds, err := t.db.PipelineDurationsMs(ctx, kind, since)
	if err != nil {
		return s, err
	}
	s.Samples = len(ds)
	s.P50 = percentile(ds, 0.50)
	s.P95 = percentile(ds, 0.95)
	s.Waiting, _, err = t.db.PipelineWaiting(ctx, kind, since)
	return s, err
}

// Percentiles reports p50 and p95 of the closed timings of kind in window,
// and how many are still waiting.
func (t *Timings) Percentiles(ctx context.Context, kind string, window time.Duration) (p50, p95 time.Duration, waiting int) {
	s, err := t.Stats(ctx, kind, window)
	if err != nil {
		return 0, 0, 0
	}
	return s.P50, s.P95, s.Waiting
}

// percentile is the nearest-rank percentile of ascending millisecond values.
func percentile(sortedMs []int64, q float64) time.Duration {
	if len(sortedMs) == 0 {
		return 0
	}
	if !sort.SliceIsSorted(sortedMs, func(i, j int) bool { return sortedMs[i] < sortedMs[j] }) {
		sortedMs = append([]int64(nil), sortedMs...)
		sort.Slice(sortedMs, func(i, j int) bool { return sortedMs[i] < sortedMs[j] })
	}
	rank := int(q*float64(len(sortedMs)) + 0.9999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sortedMs) {
		rank = len(sortedMs)
	}
	return time.Duration(sortedMs[rank-1]) * time.Millisecond
}

// Summary is what GET /api/v1/system/pipeline serves.
type Summary struct {
	Timings     []KindStats
	OverdueRows int
}

// MeasuredBy names the milestone that starts measuring a kind, or "" when it
// is measured today.
func MeasuredBy(kind string) string {
	if kind == ImportToCinemaVisible {
		return "M5"
	}
	return ""
}

// Summary assembles the three tiles and the overdue count.
func (t *Timings) Summary(ctx context.Context) (Summary, error) {
	var out Summary
	for _, k := range Kinds {
		s, err := t.Stats(ctx, k, Window)
		if err != nil {
			return out, err
		}
		out.Timings = append(out.Timings, s)
	}
	n, err := t.OverdueCount(ctx)
	out.OverdueRows = n
	return out, err
}
