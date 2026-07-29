package nzbd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Subscribe implements ports.Subscriber over nzbd's `GET /api/v1/events`
// SSE stream.
//
// Written against a raw HTTP client rather than an EventSource library for
// one blunt reason: the stream needs an `Authorization` header, and the
// EventSource standard has no way to set one. The protocol is small enough
// that the parser below is shorter than the shim would be.
//
// The contract this implements (nzbd/docs/INTEGRATION_PLAN.md §N2):
//
//   - Every engine frame carries `id: <boot>-<seq>`. It is opaque — echo
//     it back verbatim in `Last-Event-ID` and the daemon replays exactly
//     what was missed.
//   - `event: reset` means the gap could not be covered (away longer than
//     the daemon's buffer, or it restarted). Poll before trusting the
//     stream again.
//   - `event: lagged` means events were dropped. Same remedy.
//   - `tick`, `hb` and `log` are the stream's own views, carry no id, and
//     are not resumable. Only `tick` is used here, for progress.
//
// Reconnection is internal and indefinite: a subscriber that gives up
// after N tries turns a nzbd restart into "push silently stopped working",
// which is the failure mode this whole channel exists to remove.
func (c *Client) Subscribe(ctx context.Context) (<-chan ports.ClientEvent, error) {
	out := make(chan ports.ClientEvent, 64)
	go func() {
		defer close(out)
		var lastID string
		backoff := time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			resumed, err := c.stream(ctx, lastID, out)
			if resumed != "" {
				lastID = resumed
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				// Every reconnect is a hole in what we know, whatever
				// caused it. Say so once, here, rather than making each
				// error path remember to.
				select {
				case out <- ports.ClientEvent{Kind: ports.EventReset}:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(backoff)):
			}
			// 1s → 60s. Fast enough that a daemon restart is invisible,
			// slow enough that a daemon that is down does not get hammered
			// by every Monarr on the network.
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
	}()
	return out, nil
}

// jitter spreads reconnects so several clients recovering from the same
// outage do not retry in lockstep.
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1)) //nolint:gosec // spread, not secrecy
}

// stream holds one connection open, translating frames until it ends.
// Returns the last event id seen (for resuming) and the error that ended
// it, if any.
func (c *Client) stream(ctx context.Context, lastID string, out chan<- ports.ClientEvent) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.cfg.URL, "/")+"/api/v1/events", nil)
	if err != nil {
		return lastID, err
	}
	c.authorize(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Nzbd-Client", clientHeader())
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	// No timeout on the stream client: the shared one would cut a healthy
	// idle connection every 30 s. Liveness comes from nzbd's own `hb`
	// frames and from the context.
	resp, err := (&http.Client{}).Do(req) //nolint:bodyclose // closed below
	if err != nil {
		return lastID, fmt.Errorf("nzbd: events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return lastID, fmt.Errorf("nzbd: events: unexpected status %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // a tick frame carries the whole queue
	var id, name string
	var data strings.Builder
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" { // end of frame
			if name != "" {
				if id != "" {
					lastID = id
				}
				for _, ev := range translate(name, data.String(), id) {
					select {
					case out <- ev:
					case <-ctx.Done():
						return lastID, nil
					}
				}
			}
			id, name = "", ""
			data.Reset()
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue // a comment keep-alive
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "event":
			name = value
		case "data":
			// The SSE spec concatenates repeated data: lines with a
			// newline. nzbd sends one line today, but a parser that
			// overwrites would silently keep only the LAST fragment of a
			// wrapped payload — decoding garbage rather than failing.
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := sc.Err(); err != nil {
		return lastID, fmt.Errorf("nzbd: events: %w", err)
	}
	return lastID, io.EOF // the daemon closed the stream; reconnect
}

// translate turns one nzbd frame into zero or more Monarr events.
//
// Zero for the frames that carry no decision (`hb`, `log`, and any event
// about the queue as a whole). One for the rest. A `tick` carries the
// entire queue, so it fans out into a progress event per job.
func translate(name, data, id string) []ports.ClientEvent {
	seq := seqOf(id)
	switch name {
	case "reset", "lagged":
		// Both mean "you may have missed something". The consumer's job
		// is identical either way: reconcile by polling.
		return []ports.ClientEvent{{Kind: ports.EventReset, Seq: seq}}

	case "job_pp_stage":
		var f struct {
			Job   int64  `json:"job"`
			Name  string `json:"name"`
			Stage string `json:"stage"`
		}
		if json.Unmarshal([]byte(data), &f) != nil {
			return nil
		}
		return []ports.ClientEvent{{
			Handle: handleOf(f.Job), Kind: ports.EventStage, Seq: seq,
			Stage: f.Stage,
			Status: ports.DownloadStatus{
				Handle: handleOf(f.Job), Name: f.Name,
				State:   ports.StateDownloading,
				Message: "post-processing: " + strings.ReplaceAll(f.Stage, "_", " "),
				Stage:   f.Stage,
			},
		}}

	case "job_pp_finished":
		var f struct {
			Job      int64  `json:"job"`
			Name     string `json:"name"`
			PpStatus string `json:"pp_status"`
			FinalDir string `json:"final_dir"`
		}
		if json.Unmarshal([]byte(data), &f) != nil {
			return nil
		}
		// This is the event the whole integration is for: post-processing
		// is done, the history row is already written, and the path is
		// attached. It is the difference between importing now and
		// importing up to 30 seconds from now.
		st := statusOfHistory(historyEntry{
			Job: f.Job, Name: f.Name, Status: f.PpStatus, FinalDir: f.FinalDir,
		})
		kind := ports.EventCompleted
		if st.State != ports.StateCompleted {
			kind = ports.EventFailed
		}
		return []ports.ClientEvent{{Handle: st.Handle, Kind: kind, Seq: seq, Status: st}}

	case "job_deleted":
		var f struct {
			Job int64 `json:"job"`
		}
		if json.Unmarshal([]byte(data), &f) != nil {
			return nil
		}
		// The same mapping the history path uses, on purpose. This branch
		// used to produce a plain StateFailed with no Blameless, so which of
		// the two channels delivered a deletion decided whether the release
		// got blocklisted — push banned it, poll did not. Two channels for
		// one fact must not disagree about what the fact means.
		return []ports.ClientEvent{{
			Handle: handleOf(f.Job), Kind: ports.EventRemoved, Seq: seq,
			Status: ports.DownloadStatus{
				Handle: handleOf(f.Job), State: ports.StateRemoved,
				Message: "removed in nzbd", Blameless: true,
			},
		}}

	case "tick":
		// The 1 Hz read model. Progress only — `job_finished` fires when
		// the DOWNLOAD ends, before post-processing, so treating anything
		// here as completion would import a folder that is still being
		// unpacked.
		var f struct {
			Jobs []job `json:"jobs"`
		}
		if json.Unmarshal([]byte(data), &f) != nil {
			return nil
		}
		evs := make([]ports.ClientEvent, 0, len(f.Jobs))
		for _, j := range f.Jobs {
			state, msg, stage := j.state()
			if state == ports.StateCompleted {
				continue // not ours to declare; wait for job_pp_finished
			}
			progress := 0.0
			if j.SizeBytes > 0 {
				progress = float64(j.DownloadedBytes) / float64(j.SizeBytes)
			}
			evs = append(evs, ports.ClientEvent{
				Handle: handleOf(j.ID), Kind: ports.EventProgress, Seq: seq,
				Status: ports.DownloadStatus{
					Handle: handleOf(j.ID), Name: j.Name, State: state,
					Progress: progress, Message: msg, Stage: stage,
				},
			})
		}
		return evs
	}
	return nil
}

func handleOf(id int64) ports.Handle { return ports.Handle(strconv.FormatInt(id, 10)) }

// seqOf pulls the sequence half out of an `<boot>-<seq>` event id. The id
// is opaque for resuming; the seq is only for saying which event a trace
// entry came from.
func seqOf(id string) uint64 {
	_, s, found := strings.Cut(id, "-")
	if !found {
		return 0
	}
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}
