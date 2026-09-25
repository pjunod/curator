package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
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
			if counts, err := s.deps.Acquisition.RecoveryMetrics(ctx); err == nil {
				fmt.Fprintln(w, "# TYPE monarr_recovery_imports gauge")
				for _, state := range []string{"queued", "importing", "review", "cancel_pending", "cancelled", "receipt_pending", "imported"} {
					fmt.Fprintf(w, "monarr_recovery_imports{state=%q} %d\n", state, counts[state])
				}
				fmt.Fprintln(w, "# TYPE monarr_recovery_outbox_pending gauge")
				fmt.Fprintf(w, "monarr_recovery_outbox_pending %d\n", counts["receipt_outbox_pending"])
			}
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

		if s.deps.Acquisition != nil {
			// The data plane, countable. Monarr had no integration counters
			// at all — the pipeline could not be joined in Prometheus from
			// this end, which for the application in the MIDDLE of it is the
			// worst place to have the gap.
			inflight := s.deps.Acquisition.Transfers()
			byStage := map[string]int{"downloading": 0, "importing": 0, "notifying": 0}
			var moving, capacity int64
			for _, t := range inflight {
				byStage[t.Stage]++
				moving += t.Bytes
				capacity += t.Total
			}
			fmt.Fprintf(w, "# HELP monarr_transfers_in_flight Work in flight, by seam.\n# TYPE monarr_transfers_in_flight gauge\n")
			for _, stage := range []string{"downloading", "importing", "notifying"} {
				fmt.Fprintf(w, "monarr_transfers_in_flight{stage=%q} %d\n", stage, byStage[stage])
			}
			fmt.Fprintf(w, "# HELP monarr_transfer_bytes_moved Bytes moved so far by in-flight transfers.\n# TYPE monarr_transfer_bytes_moved gauge\n")
			fmt.Fprintf(w, "monarr_transfer_bytes_moved %d\n", moving)
			fmt.Fprintf(w, "# HELP monarr_transfer_bytes_total Bytes those transfers are expected to move.\n# TYPE monarr_transfer_bytes_total gauge\n")
			fmt.Fprintf(w, "monarr_transfer_bytes_total %d\n", capacity)
		}
		fmt.Fprintf(w, "# HELP monarr_uptime_seconds Seconds since process start.\n# TYPE monarr_uptime_seconds counter\n")
		fmt.Fprintf(w, "monarr_uptime_seconds %.0f\n", time.Since(s.deps.StartedAt).Seconds())
	})
}
