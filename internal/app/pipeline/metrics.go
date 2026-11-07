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
func (t *Timings) WriteMetrics(ctx context.Context, w io.Writer) {
	for _, kind := range Kinds {
		name := "monarr_" + kind + "_seconds"
		var ds []int64
		if t != nil && t.db != nil {
			ds, _ = t.db.PipelineDurationsMs(ctx, kind, 0)
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, helpText[kind], name)
		var sumMs int64
		for _, d := range ds {
			sumMs += d
		}
		for _, le := range bucketsSeconds {
			n := 0
			for _, d := range ds {
				if float64(d)/1000 <= le {
					n++
				}
			}
			fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, le, n)
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, len(ds))
		fmt.Fprintf(w, "%s_sum %g\n%s_count %d\n", name, float64(sumMs)/1000, name, len(ds))
	}
	fmt.Fprintf(w, "# HELP monarr_pipeline_waiting Timings started in the last 24 h that have not finished, by kind.\n# TYPE monarr_pipeline_waiting gauge\n")
	for _, kind := range Kinds {
		s, _ := t.Stats(ctx, kind, Window)
		fmt.Fprintf(w, "monarr_pipeline_waiting{kind=%q} %d\n", kind, s.Waiting)
	}
}
