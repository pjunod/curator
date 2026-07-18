package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
)

// metricsHandler serves optional Prometheus text exposition at /metrics
// (Phase 5). Hand-rolled — a few gauges don't justify a client library.
// Enable with MONARR_METRICS=true; otherwise 404 (opt-in, like the ledger
// says: "optional Prometheus /metrics").
func (s *Server) metricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := strings.ToLower(os.Getenv("MONARR_METRICS")); v != "1" && v != "true" {
			http.NotFound(w, r)
			return
		}
		ctx := r.Context()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		fmt.Fprintf(w, "# HELP monarr_build_info Build metadata.\n# TYPE monarr_build_info gauge\n")
		fmt.Fprintf(w, "monarr_build_info{version=%q,commit=%q} 1\n", s.deps.Version, s.deps.Commit)

		fmt.Fprintf(w, "# HELP monarr_media_items Library items by kind.\n# TYPE monarr_media_items gauge\n")
		for _, kind := range []domain.MediaKind{domain.KindMovie, domain.KindSeries, domain.KindBook} {
			n := 0
			if items, err := s.deps.Library.List(ctx, kind); err == nil {
				n = len(items)
			}
			fmt.Fprintf(w, "monarr_media_items{kind=%q} %d\n", kind, n)
		}

		if s.deps.Acquisition != nil {
			active := 0
			if rows, err := s.deps.Acquisition.Queue(ctx); err == nil {
				for _, d := range rows {
					if d.State == "grabbed" || d.State == "downloading" || d.State == "importing" {
						active++
					}
				}
			}
			fmt.Fprintf(w, "# HELP monarr_queue_active Downloads in flight.\n# TYPE monarr_queue_active gauge\n")
			fmt.Fprintf(w, "monarr_queue_active %d\n", active)

			wanted := 0
			if ws, err := s.deps.Acquisition.WantedList(ctx); err == nil {
				wanted = len(ws)
			}
			fmt.Fprintf(w, "# HELP monarr_wanted_total Missing or cutoff-unmet wantables.\n# TYPE monarr_wanted_total gauge\n")
			fmt.Fprintf(w, "monarr_wanted_total %d\n", wanted)
		}

		fmt.Fprintf(w, "# HELP monarr_uptime_seconds Seconds since process start.\n# TYPE monarr_uptime_seconds counter\n")
		fmt.Fprintf(w, "monarr_uptime_seconds %.0f\n", time.Since(s.deps.StartedAt).Seconds())
	})
}
