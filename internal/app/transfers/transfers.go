// Package transfers is the in-flight view of the data plane: what is moving
// right now, between which two applications, and how far along it is.
//
// It exists because that question had no answer. A 20 GB import held the
// queue poll open for seventeen minutes and the only trace of it anywhere in
// Monarr was the duration column of an unrelated scheduled task — while the
// Connections panel, reading a clock that had stopped because the poll could
// not tick, reported the download client it was busy importing FROM as
// degraded. Every part of that is the same mistake: the control plane and the
// data plane were the same goroutine, and the data plane was not instrumented
// at all.
//
// So this package holds one fact — a transfer in flight — and holds it
// separately from anything that decides. Reading it never blocks a mover;
// moving never blocks a reader. The health checks observe the control plane
// (can we still talk to these applications), and this observes the data plane
// (is anything actually moving). Neither can be mistaken for the other again.
package transfers

import (
	"context"
	"sync"
	"time"
)

// Stage is where in the pipeline a transfer currently is. These are the
// seams: each one is a boundary between two applications, or between Monarr
// and the filesystem.
const (
	// StageDownloading — nzbd is fetching it. Monarr is watching, not working.
	StageDownloading = "downloading"
	// StageImporting — Monarr is moving bytes into the library. The only
	// stage where Monarr itself is the thing doing the work, and the one
	// that was invisible.
	StageImporting = "importing"
	// StageNotifying — telling plurx an import landed, including the waits
	// between retries.
	StageNotifying = "notifying"
)

// Transfer is one thing in flight.
//
// Deliberately a value, not a pointer into live state: a snapshot a caller
// can render at leisure cannot be mutated underneath it, and a status page
// holding a lock on the import path would be the same category of mistake
// this package exists to undo.
type Transfer struct {
	// DownloadID is Monarr's row, so a reader can get from here to the
	// handoff trace. Transfer is the id that spans all three applications
	// (contract §3.1) — the one to grep with.
	DownloadID int64  `json:"downloadId"`
	Transfer   string `json:"transfer,omitempty"`
	Title      string `json:"title"`
	Stage      string `json:"stage"`
	// Peer is the application on the other side of this seam, and Outbound
	// says which way the bytes are going. "importing" has a peer of "" —
	// Monarr is moving its own files, and naming a peer there would invent
	// a seam that does not exist.
	Peer     string `json:"peer,omitempty"`
	Outbound bool   `json:"outbound"`

	StartedAt time.Time `json:"startedAt"`
	// Bytes and Total are 0 when the stage cannot measure itself. A UI must
	// render "no idea how far along" differently from "0% done", so the
	// absence is explicit rather than a zero percentage.
	Bytes int64 `json:"bytes,omitempty"`
	Total int64 `json:"total,omitempty"`
	// BytesPerSecond over the life of this transfer, not an instantaneous
	// rate: an instantaneous one is mostly noise on a network mount, and the
	// question a person is actually asking is "will this finish tonight".
	BytesPerSecond float64 `json:"bytesPerSecond,omitempty"`
	Detail         string  `json:"detail,omitempty"`
}

// Elapsed is how long this transfer has been running.
func (t Transfer) Elapsed() time.Duration { return time.Since(t.StartedAt) }

// Fraction is progress in 0..1, and whether it is knowable at all.
func (t Transfer) Fraction() (float64, bool) {
	if t.Total <= 0 || t.Bytes < 0 {
		return 0, false
	}
	f := float64(t.Bytes) / float64(t.Total)
	if f > 1 {
		f = 1
	}
	return f, true
}

// key identifies one transfer: a download can legitimately be at two stages
// at once (importing while an earlier notify is still retrying), so the
// download id alone is not enough.
type key struct {
	download int64
	stage    string
}

// Registry holds what is in flight. Safe for concurrent use; every method is
// O(number of in-flight transfers), which is bounded by the queue.
type Registry struct {
	mu sync.Mutex
	m  map[key]Transfer
}

// New returns an empty registry.
func New() *Registry { return &Registry{m: map[key]Transfer{}} }

// Begin records a transfer entering a stage, returning a handle for
// reporting progress and finishing.
//
// Beginning a stage that is already recorded replaces it rather than
// duplicating: a retry of the same stage for the same download is the same
// transfer having another go, and showing it twice would suggest two things
// are moving when one is.
func (r *Registry) Begin(t Transfer) *Handle {
	if r == nil {
		return &Handle{}
	}
	t.StartedAt = time.Now()
	r.mu.Lock()
	r.m[key{t.DownloadID, t.Stage}] = t
	r.mu.Unlock()
	return &Handle{reg: r, k: key{t.DownloadID, t.Stage}}
}

// Snapshot is everything in flight, newest first.
func (r *Registry) Snapshot() []Transfer {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]Transfer, 0, len(r.m))
	for _, t := range r.m {
		out = append(out, t)
	}
	r.mu.Unlock()
	// Stable, and oldest first: the thing that has been running longest is
	// the thing a person opening this page wants to see.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartedAt.Before(out[j-1].StartedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Count is how many transfers are in flight at a stage, or all of them when
// stage is empty. For the metrics endpoint, which should not have to
// materialise a slice to count.
func (r *Registry) Count(stage string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if stage == "" {
		return len(r.m)
	}
	n := 0
	for k := range r.m {
		if k.stage == stage {
			n++
		}
	}
	return n
}

// Oldest is how long the longest-running transfer at a stage has been going,
// and whether there is one. The health check reads this: an import that has
// been running for hours is a data-plane problem, and it is a different
// problem from a client that stopped answering.
func (r *Registry) Oldest(stage string) (time.Duration, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var start time.Time
	for k, t := range r.m {
		if stage != "" && k.stage != stage {
			continue
		}
		if start.IsZero() || t.StartedAt.Before(start) {
			start = t.StartedAt
		}
	}
	if start.IsZero() {
		return 0, false
	}
	return time.Since(start), true
}

// Handle reports on one in-flight transfer.
//
// A nil-safe zero Handle is usable and does nothing, so a caller that has no
// registry — a test, a code path wired before the registry exists — needs no
// branch around every progress call.
type Handle struct {
	reg *Registry
	k   key

	mu       sync.Mutex
	reported time.Time
}

// progressFlush bounds how often a byte count is published.
//
// Copying reports every buffer, which for a 20 GB file is tens of thousands
// of updates a minute. Nobody reads a status page that often, and taking a
// mutex on the copy loop that often is a cost paid by the thing we are trying
// not to slow down. Matches the coalescing the push subscriber already does
// for download progress, for the same reason.
const progressFlush = 500 * time.Millisecond

// Bytes reports progress. Cheap to call from a copy loop: updates more
// frequent than progressFlush are dropped, except the final one.
func (h *Handle) Bytes(done, total int64) {
	if h == nil || h.reg == nil {
		return
	}
	h.mu.Lock()
	if done < total && time.Since(h.reported) < progressFlush {
		h.mu.Unlock()
		return
	}
	h.reported = time.Now()
	h.mu.Unlock()

	h.reg.mu.Lock()
	defer h.reg.mu.Unlock()
	t, ok := h.reg.m[h.k]
	if !ok {
		return
	}
	t.Bytes, t.Total = done, total
	if secs := time.Since(t.StartedAt).Seconds(); secs > 0.5 {
		t.BytesPerSecond = float64(done) / secs
	}
	h.reg.m[h.k] = t
}

// Detail replaces the one-line description of what this transfer is doing.
func (h *Handle) Detail(s string) {
	if h == nil || h.reg == nil {
		return
	}
	h.reg.mu.Lock()
	defer h.reg.mu.Unlock()
	if t, ok := h.reg.m[h.k]; ok {
		t.Detail = s
		h.reg.m[h.k] = t
	}
}

// End removes the transfer. Safe to call twice, and safe to defer.
func (h *Handle) End() {
	if h == nil || h.reg == nil {
		return
	}
	h.reg.mu.Lock()
	delete(h.reg.m, h.k)
	h.reg.mu.Unlock()
}

// ---- carrying a handle through the import ----

type ctxKey struct{}

// WithHandle puts a transfer handle on the context, so the code that
// eventually copies bytes can report them without every function between
// here and there growing a parameter it does not otherwise care about.
//
// A context value is the right tool exactly once: this is request-scoped
// diagnostics, it changes no behaviour, and its absence is a working
// no-op. Anything that decides something belongs in an argument.
func WithHandle(ctx context.Context, h *Handle) context.Context {
	return context.WithValue(ctx, ctxKey{}, h)
}

// Progress returns a byte-reporting callback for whatever transfer this
// context belongs to, or nil when it belongs to none.
func Progress(ctx context.Context) func(done, total int64) {
	h, _ := ctx.Value(ctxKey{}).(*Handle)
	if h == nil {
		return nil
	}
	return h.Bytes
}
