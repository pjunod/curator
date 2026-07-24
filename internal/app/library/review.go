package library

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/ports"
)

// reviewQueueKey holds the proposals the last adoption run could not apply.
// Persisted so the review list survives a page reload and a restart: a
// queue you have to regenerate by re-running provider searches is not a
// queue, it is a report.
const reviewQueueKey = "adoption_review_queue"

// ReviewPage is one window onto the review queue.
//
// A real library produces hundreds of these, so the queue is paginated at
// the source rather than rendered in full and hidden with CSS: the settings
// page should not download six hundred proposals to display a count.
type ReviewPage struct {
	Items  []Proposal `json:"items"`
	Total  int        `json:"total"` // matching the filter
	Offset int        `json:"offset"`
	Limit  int        `json:"limit"`
	// Counts are over the whole queue, unfiltered, so the tab labels stay
	// stable while the user narrows the list.
	Counts ReviewCounts `json:"counts"`
}

// ReviewCounts summarises the queue by confidence and by kind, so tab
// labels can carry their own totals without a request per tab.
type ReviewCounts struct {
	Total     int `json:"total"`
	Ambiguous int `json:"ambiguous"`
	None      int `json:"none"`
	Movie     int `json:"movie"`
	Series    int `json:"series"`
	Book      int `json:"book"`
	// Unknown counts entries from mixed roots, where no kind was resolved.
	Unknown int `json:"unknown"`
}

// defaultReviewLimit is one screenful. Deliberately modest: the point of
// paginating is that a folder of 600 movies does not arrive at once.
const defaultReviewLimit = 25

// maxReviewLimit caps what a caller can ask for in one page.
const maxReviewLimit = 200

// saveReviewQueue persists what needs a human. Failure to persist is logged
// rather than fatal — the adoption itself already succeeded, and losing the
// queue only means the next scan rebuilds it.
func (s *Service) saveReviewQueue(ctx context.Context, proposals []Proposal) {
	raw, err := json.Marshal(proposals)
	if err != nil {
		s.log.Warn("adopt: could not encode review queue", "err", err)
		return
	}
	if err := s.db.SetMeta(ctx, reviewQueueKey, string(raw)); err != nil {
		s.log.Warn("adopt: could not persist review queue", "err", err)
	}
}

// loadReviewQueue returns the persisted proposals, falling back to the raw
// unmatched directories from the last scan when adoption has not run yet.
// The fallback matters: without it, a user who scans but has not pressed
// match sees an empty review list and concludes the scan found nothing.
func (s *Service) loadReviewQueue(ctx context.Context) []Proposal {
	if raw, err := s.db.GetMeta(ctx, reviewQueueKey); err == nil && raw != "" {
		var out []Proposal
		if err := json.Unmarshal([]byte(raw), &out); err == nil {
			return out
		}
		s.log.Warn("adopt: review queue unreadable, falling back to the scan report")
	}

	report, ok, err := s.LastScanReport(ctx)
	if err != nil || !ok {
		return nil
	}
	out := make([]Proposal, 0, len(report.UnmatchedDirs))
	for _, d := range report.UnmatchedDirs {
		// Parse even in the fallback. It costs nothing (no provider call)
		// and it is the difference between the row saying
		// "read as Arrival (2016)" and repeating the raw folder name back
		// at the user, which tells them nothing they cannot already see.
		parsed := parser.Parse(d.Name)
		title := parsed.Title
		if title == "" {
			title = d.Name
		}
		out = append(out, Proposal{
			RootFolderID: d.RootFolderID,
			Path:         d.Path,
			Name:         d.Name,
			ParsedTitle:  title,
			ParsedYear:   parsed.Year,
			Confidence:   ConfidenceNone,
			Candidates:   []ports.SearchResult{},
		})
	}
	return out
}

// ReviewQueue returns one page of the adoption review queue, optionally
// filtered by media kind and by a free-text query over the folder name and
// parsed title.
//
// A limit of 0 means "all", which is the escape hatch for anyone who would
// rather scroll or use the browser's own find. It is honoured rather than
// clamped, because refusing it would just push people to click Next forty
// times.
//
// Dismissed paths are filtered out on read rather than rewritten into the
// stored queue: a dismissal is one small write, and rewriting a
// six-hundred-entry blob on every click is the kind of thing that is fine
// until it is not.
func (s *Service) ReviewQueue(ctx context.Context, kind domain.MediaKind, q string, limit, offset int) ReviewPage {
	all := limit == 0
	if limit < 0 {
		limit = defaultReviewLimit
	}
	if limit > maxReviewLimit {
		limit = maxReviewLimit
	}
	if offset < 0 {
		offset = 0
	}

	queue := s.loadReviewQueue(ctx)

	ignored := map[string]bool{}
	if rows, err := s.db.ListIgnoredPaths(ctx); err == nil {
		for _, ip := range rows {
			ignored[ip.Path] = true
		}
	}

	page := ReviewPage{Items: []Proposal{}, Limit: limit, Offset: offset}
	needle := strings.ToLower(strings.TrimSpace(q))
	matched := make([]Proposal, 0, len(queue))
	for _, p := range queue {
		if ignored[p.Path] {
			continue
		}
		// Counts are over the whole queue, before kind and text filters, so
		// the tab labels stay still while the user narrows the list.
		page.Counts.Total++
		switch p.Confidence {
		case ConfidenceAmbiguous:
			page.Counts.Ambiguous++
		case ConfidenceNone:
			page.Counts.None++
		case ConfidenceExact:
			// An exact proposal in the review queue is waiting on a
			// first-pass confirmation — not a state the counts split out.
		}
		switch p.Kind {
		case domain.KindMovie:
			page.Counts.Movie++
		case domain.KindSeries:
			page.Counts.Series++
		case domain.KindBook:
			page.Counts.Book++
		default:
			page.Counts.Unknown++ // mixed root: kind never resolved
		}

		if kind != "" && p.Kind != kind {
			continue
		}
		if needle != "" &&
			!strings.Contains(strings.ToLower(p.Name), needle) &&
			!strings.Contains(strings.ToLower(p.ParsedTitle), needle) {
			continue
		}
		matched = append(matched, p)
	}

	page.Total = len(matched)
	if all {
		page.Items = matched
		page.Limit = 0
		return page
	}
	if offset >= len(matched) {
		return page
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	page.Items = matched[offset:end]
	return page
}
