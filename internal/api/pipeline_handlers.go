package api

import (
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/pipeline"
)

// PipelineSummary implements GET /system/pipeline: the last-24-hour p50/p95 of
// the three seams between Runner, Curator and Cinema, with what is still
// waiting and how many downloads have outlived their state's allowance.
func (s *Server) PipelineSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.deps.Pipeline.Summary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read pipeline timings: "+err.Error())
		return
	}
	out := apigen.PipelineSummary{
		WindowHours: int(pipeline.Window.Hours()),
		OverdueRows: sum.OverdueRows,
		Timings:     []apigen.PipelineTiming{},
	}
	for _, k := range sum.Timings {
		t := apigen.PipelineTiming{
			Kind:    apigen.PipelineTimingKind(k.Kind),
			Samples: k.Samples,
			Waiting: k.Waiting,
		}
		if k.Samples > 0 {
			p50, p95 := k.P50.Seconds(), k.P95.Seconds()
			t.P50Seconds, t.P95Seconds = &p50, &p95
		}
		out.Timings = append(out.Timings, t)
	}
	writeJSON(w, http.StatusOK, out)
}
