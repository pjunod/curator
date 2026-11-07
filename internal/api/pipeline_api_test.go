package api

import (
	"net/http"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

func TestMetricsListsPipelineFamiliesAndCounters(t *testing.T) {
	e := newAPIEnv(t)
	t.Setenv("MONARR_METRICS", "true")
	body := e.get(t, "/metrics").expect(t, http.StatusOK).Body.String()
	for _, want := range []string{
		"# TYPE monarr_failure_to_next_grab_seconds histogram",
		"# TYPE monarr_runner_done_to_import_seconds histogram",
		"# TYPE monarr_import_to_cinema_visible_seconds histogram",
		"monarr_pipeline_overdue_rows 0",
		"monarr_runner_fact_mismatch_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}

func TestPipelineSummaryReportsUnmeasuredSeam(t *testing.T) {
	e := newAPIEnv(t)
	var sum apigen.PipelineSummary
	e.get(t, "/api/v1/system/pipeline").expect(t, http.StatusOK).into(t, &sum)
	if sum.WindowHours != 24 || sum.OverdueRows != 0 || len(sum.Timings) != 3 {
		t.Fatalf("summary = %+v, want a 24 h window, no overdue rows, three seams", sum)
	}
	last := sum.Timings[2]
	if last.Kind != apigen.ImportToCinemaVisible || last.Measured || last.Note == nil || !strings.Contains(*last.Note, "M5") {
		t.Errorf("import_to_cinema_visible = %+v, want unmeasured and naming M5", last)
	}
	if first := sum.Timings[0]; !first.Measured || first.P50Seconds != nil || first.Samples != 0 {
		t.Errorf("failure_to_next_grab with no data = %+v, want measured with no percentiles", first)
	}
}
