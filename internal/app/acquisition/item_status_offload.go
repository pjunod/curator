package acquisition

import (
	"context"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// The "Offloading files" block of the item status panel (ADR 0028): the open
// Offload deleting this title's files, and once it is over the final stage
// "Offloaded <date> · <bytes> freed" until it is dismissed. It adds nothing to
// the operation; it makes the work visible where the title is.

// StatusOffload is one Offload as the item page shows it.
type StatusOffload struct {
	OperationID string `json:"operationId"`
	State       string `json:"state"`
	Scope       string `json:"scope"`
	Label       string `json:"label"`
	// Headline is the stage in words: what is happening, or the final
	// "Offloaded <date> · <bytes> freed".
	Headline   string `json:"headline"`
	FilesDone  int    `json:"filesDone"`
	FilesTotal int    `json:"filesTotal"`
	BytesTotal int64  `json:"bytesTotal"`
	BytesFreed int64  `json:"bytesFreed"`
	// Failed are the files that could not be deleted yet and why; the
	// operation retries them (state retrying).
	Failed []string `json:"failed,omitempty"`
	// Error is the plain reason the operation is waiting or retrying.
	Error      string          `json:"error,omitempty"`
	Cinema     []RematchCinema `json:"cinema,omitempty"`
	CinemaLine string          `json:"cinemaLine,omitempty"`
	// Ended: the operation is over (complete, stopped or failed); dismiss it
	// with POST /offload/{id}/dismiss.
	Ended     bool `json:"ended"`
	Stoppable bool `json:"stoppable"`
	// FinishedAt is when it ended, unix milliseconds.
	FinishedAt int64 `json:"finishedAt,omitempty"`
}

func (o *StatusOffload) running() bool { return o != nil && !o.Ended }

// offloadFacts reads the open (or newest undismissed) Offload of the item.
func (s *Service) offloadFacts(ctx context.Context, itemID int64) (*OffloadStatus, error) {
	st, ok, err := s.OffloadForItem(ctx, itemID)
	if err != nil || !ok {
		return nil, err
	}
	return &st, nil
}

func (f statusFacts) offloadBlock() *StatusOffload {
	op := f.offload
	if op == nil {
		return nil
	}
	o := &StatusOffload{
		OperationID: op.ID, State: op.State, Scope: op.Scope, Label: op.Label,
		FilesDone: op.FilesDone, FilesTotal: op.FilesTotal, BytesTotal: op.BytesTotal, BytesFreed: op.BytesFreed,
		Failed: op.Failed, Error: op.LastError, Cinema: op.Cinema, CinemaLine: op.CinemaLine,
		Ended: !sqlite.OffloadOpen(op.State), Stoppable: op.Stoppable,
	}
	what := op.Label
	if what == "" {
		what = titleWithYear(f.item)
	}
	if o.Ended {
		o.FinishedAt = op.UpdatedAt
	}
	switch op.State {
	case "deleting":
		o.Headline = fmt.Sprintf("Offloading %s: %d of %d files deleted", what, op.FilesDone, op.FilesTotal)
	case "waiting":
		o.Headline = "Offload of " + what + " is waiting"
	case "retrying":
		o.Headline = fmt.Sprintf("Offload of %s: %d file(s) could not be deleted; retrying", what, len(op.Failed))
	case "cancelled":
		o.Headline = fmt.Sprintf("Offload of %s was stopped; %d of %d files were deleted", what, op.FilesDone, op.FilesTotal)
	case "failed":
		o.Headline = "Offload of " + what + " failed"
	default:
		when := time.UnixMilli(op.UpdatedAt).Format("2 Jan 2006")
		o.Headline = fmt.Sprintf("Offloaded %s · %s freed", when, humanBytes(op.BytesFreed))
	}
	return o
}
