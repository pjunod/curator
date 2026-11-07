package pipeline

import (
	"context"
	"fmt"
	"io"
)

// bucketsSeconds are the fixed histogram upper bounds (contract section 3.5).
var bucketsSeconds = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}

var helpText = map[string]string{
	FailureToNextGrab:     "Seconds from a download failing to its replacement grab being submitted.",
	RunnerDoneToImport:    "Seconds from the client reporting the payload finished to the library import completing.",
	ImportToCinemaVisible: "Seconds from an import completing to Cinema reporting the files placed (measured from pipeline M5).",
}

// WriteMetrics writes the three duration histograms and the waiting gauges
// in Prometheus text format, built from every closed timing in the table.
// The overdue-rows gauge is written by the caller's /metrics handler next to
// the other counters it owns.
//
// A family whose query fails is left out of the scrape entirely rather than
// written as zeros: Prometheus reads a zeroed cumulative histogram as a
// counter reset, which would poison every rate() over the window and be
// indistinguishable from an empty table. Absence is alertable; each failure is
// logged and counted in monarr_pipeline_scrape_errors_total.
func (t *Timings) WriteMetrics(ctx context.Context, w io.Writer) {
	for _, kind := range Kinds {
		name := "monarr_" + kind + "_seconds"
		cum := make([]int64, len(bucketsSeconds))
		var sumMs, count int64
		if t != nil && t.db != nil {
			bounds := make([]int64, len(bucketsSeconds))
			for i, le := range bucketsSeconds {
				bounds[i] = int64(le * 1000)
			}
			c, sm, n, err := t.db.PipelineHistogram(ctx, kind, bounds)
			if err != nil {
				t.scrapeError("histogram "+kind, err)
				continue
			}
			cum, sumMs, count = c, sm, n
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, helpText[kind], name)
		for i, le := range bucketsSeconds {
			fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, le, cum[i])
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, count)
		fmt.Fprintf(w, "%s_sum %g\n%s_count %d\n", name, float64(sumMs)/1000, name, count)
	}
	fmt.Fprintf(w, "# HELP monarr_pipeline_waiting Timings started in the last 24 h that have not finished, by kind.\n# TYPE monarr_pipeline_waiting gauge\n")
	for _, kind := range Kinds {
		var waiting int
		if t != nil && t.db != nil {
			n, _, err := t.db.PipelineWaiting(ctx, kind, t.now().Add(-Window).UnixMilli())
			if err != nil {
				t.scrapeError("waiting "+kind, err)
				continue
			}
			waiting = n
		}
		fmt.Fprintf(w, "monarr_pipeline_waiting{kind=%q} %d\n", kind, waiting)
	}
	fmt.Fprintf(w, "# HELP monarr_pipeline_scrape_errors_total Pipeline metric queries that failed during a scrape; the affected series were omitted.\n# TYPE monarr_pipeline_scrape_errors_total counter\n")
	fmt.Fprintf(w, "monarr_pipeline_scrape_errors_total %d\n", t.ScrapeErrors())
}

// scrapeError records a failed metrics query.
func (t *Timings) scrapeError(what string, err error) {
	t.scrapeErrors.Add(1)
	if t.log != nil {
		t.log.Warn("pipeline metrics: query failed; series omitted from this scrape", "query", what, "err", err)
	}
}

// ScrapeErrors is the number of failed metrics queries since start.
func (t *Timings) ScrapeErrors() int64 {
	if t == nil {
		return 0
	}
	return t.scrapeErrors.Load()
}
