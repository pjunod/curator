package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// The "Moving files" block of the item status panel: the open Re-match that
// moves files out of or into the item, from the operation's own rows. It
// adds nothing to the Re-match; it makes the move visible where the item is.

// StatusRematch is one open Re-match as the item page shows it.
type StatusRematch struct {
	OperationID  string `json:"operationId"`
	State        string `json:"state"`
	Role         string `json:"role"` // source | target: which side this item is
	SourceItemID int64  `json:"sourceItemId"`
	TargetItemID int64  `json:"targetItemId"`
	SourceTitle  string `json:"sourceTitle"`
	TargetTitle  string `json:"targetTitle"`
	// Headline is the direction in words, for this item's side.
	Headline   string `json:"headline"`
	BytesDone  int64  `json:"bytesDone"`
	BytesTotal int64  `json:"bytesTotal"`
	FilesDone  int    `json:"filesDone"`
	FilesTotal int    `json:"filesTotal"`
	// Moves are the from → to of each file.
	Moves []StatusRematchMove `json:"moves,omitempty"`
	// Cinema is what Cinema said to each sub-delivery; CinemaLine is the
	// one-line reading of it; Warnings are its retained/present lines.
	Cinema     []RematchCinema `json:"cinema,omitempty"`
	CinemaLine string          `json:"cinemaLine,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
	Error      string          `json:"error,omitempty"`
	// Ended: the operation is over but ended badly (failed, or complete with
	// files that could not move); it shows for rematchEndedFor or until
	// dismissed. FailedMoves are the files that did not move and why.
	Ended       bool                `json:"ended"`
	FailedMoves []StatusRematchFail `json:"failedMoves,omitempty"`
	// Stoppable is true until the files are committed (POST /rematch/{id}/cancel).
	Stoppable bool `json:"stoppable"`
}

// StatusRematchFail is a file that did not move.
type StatusRematchFail struct {
	From   string `json:"from"`
	Reason string `json:"reason"`
}

// rematchEndedFor is how long a badly ended Re-match stays on the panel.
const rematchEndedFor = 24 * time.Hour

// StatusRematchMove is one file's from → to and where it is.
type StatusRematchMove struct {
	From  string `json:"from"`
	To    string `json:"to"`
	State string `json:"state"` // planned | moved | committed | done | rolled_back | failed
	Bytes int64  `json:"bytes"`
}

// rematchFacts is what the projection reads of the open operation.
type rematchFacts struct {
	op          RematchStatus
	moves       []sqlite.RematchMove
	sourceTitle string
	targetTitle string
	itemID      int64
	ended       bool
	failed      []RematchPair
}

func (r *StatusRematch) running() bool { return r != nil && !r.Ended && rematchRunning(r.State) }

func rematchRunning(state string) bool {
	switch state {
	case "planned", "moving", "moved", "committed", "cancel_pending":
		return true
	}
	return false
}

func titleWithYear(it domain.MediaItem) string {
	if it.Year > 0 {
		return fmt.Sprintf("%s (%d)", it.Title, it.Year)
	}
	return it.Title
}

// rematchFacts reads the open operation for the item, nil when there is none.
func (s *Service) rematchFacts(ctx context.Context, itemID int64, item domain.MediaItem) (*rematchFacts, error) {
	id, err := s.db.OpenRematchForItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	ended := false
	if id == "" {
		// No open operation: the newest finished one still speaks when it
		// ended badly, until dismissed or a day old.
		done, ok, err := s.db.LatestEndedRematchForItem(ctx, itemID, time.Now().Add(-rematchEndedFor).UnixMilli())
		if err != nil || !ok {
			return nil, err
		}
		var res rematchResult
		_ = json.Unmarshal(done.Result, &res)
		badly := done.State == "failed" || (done.State == "complete" && len(res.Failed) > 0) || (done.State == "cancelled" && done.LastError != "")
		if !badly {
			return nil, nil
		}
		id, ended = done.ID, true
	}
	op, err := s.GetRematch(ctx, id)
	if errors.Is(err, sqlite.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	moves, err := s.db.ListRematchMoves(ctx, id)
	if err != nil {
		return nil, err
	}
	rf := &rematchFacts{op: op, moves: moves, itemID: itemID, ended: ended}
	if ended {
		var res rematchResult
		_ = json.Unmarshal(op.Result, &res)
		rf.failed = res.Failed
	}
	title := func(other int64) string {
		if other == itemID {
			return titleWithYear(item)
		}
		o, err := s.db.GetMediaItemFull(ctx, other)
		if err != nil {
			return "another title"
		}
		return titleWithYear(o)
	}
	rf.sourceTitle, rf.targetTitle = title(op.SourceItemID), title(op.TargetItemID)
	return rf, nil
}

func (f statusFacts) rematchBlock() *StatusRematch {
	rf := f.rematch
	if rf == nil {
		return nil
	}
	op := rf.op
	r := &StatusRematch{
		OperationID: op.ID, State: op.State, Role: "source",
		SourceItemID: op.SourceItemID, TargetItemID: op.TargetItemID,
		SourceTitle: rf.sourceTitle, TargetTitle: rf.targetTitle,
		BytesDone: op.BytesDone, BytesTotal: op.BytesTotal, Error: op.LastError,
		Stoppable: op.State == "planned" || op.State == "moving" || op.State == "moved",
		Cinema:    op.Cinema,
	}
	if op.TargetItemID == rf.itemID && op.SourceItemID != rf.itemID {
		r.Role = "target"
		r.Headline = "Files moving here from " + rf.sourceTitle
	} else {
		r.Headline = "Files moving from here to " + rf.targetTitle
	}
	if rf.ended {
		r.Ended, r.Stoppable = true, false
		for _, f := range rf.failed {
			r.FailedMoves = append(r.FailedMoves, StatusRematchFail{From: f.From, Reason: f.To})
		}
		switch op.State {
		case "failed":
			r.Headline = "Moving files from " + rf.sourceTitle + " to " + rf.targetTitle + " failed; every file is back where it was"
		case "cancelled":
			r.Headline = "Moving files from " + rf.sourceTitle + " to " + rf.targetTitle + " was stopped"
		default:
			r.Headline = fmt.Sprintf("Files moved from %s to %s; %d could not be moved", rf.sourceTitle, rf.targetTitle, len(rf.failed))
		}
	}
	for _, m := range rf.moves {
		st := strings.TrimPrefix(m.State, "rematch_")
		r.Moves = append(r.Moves, StatusRematchMove{From: m.Source, To: m.Target, State: st, Bytes: m.Size})
		r.FilesTotal++
		switch st {
		case "moved", "committed", "done":
			r.FilesDone++
		}
	}
	if len(op.Cinema) > 0 {
		confirmed := true
		seen := map[string]bool{}
		var said []string
		for _, c := range op.Cinema {
			confirmed = confirmed && c.VisibleAt > 0
			for _, w := range c.Warnings {
				if !seen[w] {
					seen[w] = true
					r.Warnings = append(r.Warnings, w)
				}
			}
			if c.Result != "" {
				said = append(said, c.Result)
			}
		}
		line := "Cinema is being told."
		if confirmed {
			line = "In cinema."
		}
		r.CinemaLine = strings.TrimSpace(line + " " + strings.Join(said, "; "))
	}
	return r
}
