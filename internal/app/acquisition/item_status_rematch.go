package acquisition

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// Stoppable is true until the files are committed (POST /rematch/{id}/cancel).
	Stoppable bool `json:"stoppable"`
}

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
}

func (r *StatusRematch) running() bool { return r != nil && rematchRunning(r.State) }

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
	if err != nil || id == "" {
		return nil, err
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
	rf := &rematchFacts{op: op, moves: moves, itemID: itemID}
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

// cinemaWarnings picks the retained/present lines out of Cinema's answers.
func cinemaWarnings(c []RematchCinema) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range c {
		for _, raw := range []string{d.Result, d.LastError} {
			for _, part := range strings.Split(raw, "; ") {
				p := strings.TrimSpace(part)
				if (strings.HasPrefix(p, "cinema still has") || strings.HasPrefix(p, "cinema kept") || strings.HasPrefix(p, "cinema is holding")) && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
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
		r.Warnings = cinemaWarnings(op.Cinema)
		confirmed := true
		var said []string
		for _, c := range op.Cinema {
			confirmed = confirmed && c.VisibleAt > 0
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
