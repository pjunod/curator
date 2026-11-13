package acquisition

import (
	"context"
	"sort"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type ImportActivityRow struct {
	sqlite.ImportActivityEntry
	Category string
}

type ImportActivityPage struct {
	Rows    []ImportActivityRow
	Counts  map[string]int
	Total   int
	HasMore bool
}

// ImportActivity reads only local records and worker ownership. No filesystem
// probes or admission are performed by a status request.
func (s *Service) ImportActivity(ctx context.Context, filter, search string, limit, offset int) (ImportActivityPage, error) {
	entries, err := s.db.ImportActivityIndex(ctx, search)
	if err != nil {
		return ImportActivityPage{}, err
	}
	page := ImportActivityPage{Rows: []ImportActivityRow{}, Counts: map[string]int{"all": 0, "active": 0, "queued": 0, "attention": 0, "imported": 0, "history": 0}}
	s.importMu.Lock()
	for _, e := range entries {
		status := ""
		if t := s.importJobs[e.ID]; t != nil {
			status = "running"
			if t.queued {
				status = "queued"
			}
		}
		cat := importActivityCategory(e, status)
		page.Counts[cat]++
		page.Counts["all"]++
		if filter == "all" || filter == cat {
			page.Rows = append(page.Rows, ImportActivityRow{ImportActivityEntry: e, Category: cat})
		}
	}
	s.importMu.Unlock()
	// Preserve update order within each category; active and waiting work always
	// precede history, even when a newer completed download arrives.
	rank := map[string]int{"active": 0, "queued": 1, "attention": 2, "imported": 3, "history": 4}
	sort.SliceStable(page.Rows, func(i, j int) bool { return rank[page.Rows[i].Category] < rank[page.Rows[j].Category] })
	page.Total = len(page.Rows)
	if offset > len(page.Rows) {
		offset = len(page.Rows)
	}
	end := min(offset+limit, len(page.Rows))
	page.HasMore = end < len(page.Rows)
	page.Rows = page.Rows[offset:end]
	return page, nil
}

func importActivityCategory(e sqlite.ImportActivityEntry, worker string) string {
	if worker == "running" {
		return "active"
	}
	if worker == "queued" {
		return "queued"
	}
	if e.Review || e.Unresolved || e.OpenFailedPublication || e.ReservationActionable || e.ActionableFiles || e.State == "awaiting_import" {
		return "attention"
	}
	if cancelledImportIsHistory(e) {
		return "history"
	}
	if e.Cancelled {
		return "attention"
	}
	if e.PendingPublication || e.State == "downloaded" {
		return "queued"
	}
	if e.ImportedPublication || e.State == "imported" {
		return "imported"
	}
	return "attention"
}

// Activity uses one complete local index before filtering, counting and paging.
// It never combines independently paged queue/import responses in the browser.
func (s *Service) Activity(ctx context.Context, view, search string, importsOnly bool, limit, offset int) (ImportActivityPage, error) {
	entries, err := s.db.ActivityIndex(ctx, search, importsOnly)
	if err != nil {
		return ImportActivityPage{}, err
	}
	page := ImportActivityPage{Rows: []ImportActivityRow{}, Counts: map[string]int{"progress": 0, "attention": 0, "history": 0, "all": 0}}
	s.importMu.Lock()
	for _, entry := range entries {
		worker := ""
		if ticket := s.importJobs[entry.ID]; ticket != nil {
			worker = "running"
		}
		category := activityCategory(entry, worker)
		page.Counts[category]++
		page.Counts["all"]++
		if category == view {
			page.Rows = append(page.Rows, ImportActivityRow{ImportActivityEntry: entry, Category: category})
		}
	}
	s.importMu.Unlock()
	page.Total = len(page.Rows)
	offset = min(max(0, offset), page.Total)
	end := min(offset+limit, page.Total)
	page.HasMore = end < page.Total
	page.Rows = page.Rows[offset:end]
	return page, nil
}

func activityCategory(e sqlite.ImportActivityEntry, worker string) string {
	// Historical publication IDs are evidence, never mutable queue targets.
	if e.ID < 0 {
		return "history"
	}
	// An active/cancelling writer keeps custody; do not offer disposition early.
	if worker != "" {
		return "progress"
	}
	if e.Review || e.State == "awaiting_import" || e.ReservationActionable || e.ActionableFiles {
		return "attention"
	}
	if e.CancelOutcome == "pending" {
		return "progress"
	}
	if e.CancelOutcome == "cancelled" {
		if cancelledImportIsHistory(e) {
			return "history"
		}
		return "attention"
	}
	if e.Superseded {
		return "history"
	}
	if e.AutomaticImportRetry {
		return "progress"
	}
	if e.LocalImportFailure && !e.FailedPublication {
		return "attention"
	}
	// A terminal transfer failure remains a failure even after salvage placement.
	if e.State == "failed" {
		return "history"
	}
	if e.PendingPublication {
		return "progress"
	}
	if e.State == "imported" || e.State == "notified" {
		return "history"
	}
	return "progress"
}

// Cancellation is historical only after every indexed custody obligation has
// cleared. Old failure text and removed publication receipts remain audit, not
// evidence of a successful import. FailedPublication includes removed failures
// for history classification; only OpenFailedPublication blocks disposition.
// Worker ownership is checked by both callers.
func cancelledImportIsHistory(e sqlite.ImportActivityEntry) bool {
	return e.CancelOutcome == "cancelled" && !e.Review && !e.Unresolved &&
		!e.PendingPublication && !e.OpenFailedPublication && !e.ReservationActionable &&
		!e.ActionableFiles && e.State != "awaiting_import"
}
