package acquisition

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// importTargets is the publication authority: who may place into, remove
// from, or decide about a media item's library files and the payloads that
// feed them.
//
// It used to be one process-wide mutex. Every import holds it for its whole
// run — hashing, copying, verifying — so one 68 GB remux being placed over a
// busy NFS link held up every other title's import and every payload
// cleanup for hours (nuc3, 2026-11-01). Imports of different titles never
// touch the same destination or the same payload, so the authority is now
// per item:
//
//   - lockImportTarget(item) holds the global authority shared and the
//     item's own lock exclusively. Different items proceed concurrently;
//     work on the same item is still serialized exactly as before. Work
//     with no item (an unassociated receipt) shares item key 0.
//   - lockAllImportTargets() holds the global authority exclusively, for work
//     that is not scoped to one item: operator deletes addressed by
//     completed-folder path, recovery (whose preview is leased against the
//     whole library's revision), and crash reconciliation of the placement
//     journal at start.
//
// Exclusive acquisition never queues. sync.RWMutex makes new readers wait
// behind a waiting writer, so one queued exclusive request would stop every
// title behind the slowest placement in flight — the original failure.
// lockAllImportTargets instead polls TryLock until no title is held, within
// a bounded wait, and reports busy rather than hanging.
//
// Neither lock is reentrant. A holder passes the scope it holds down through
// targetHeldKey; nested work on a covered scope does not lock again, and
// asking for the global authority while holding a title is refused. No path
// takes two item locks. The storage-decision lock is still the single
// serializer for the short final publish-and-commit window.
type importTargets struct {
	all   sync.RWMutex
	mu    sync.Mutex
	items map[int64]*importTargetLock
}

type importTargetLock struct {
	mu   sync.Mutex
	refs int
}

// heldImportTarget is the scope a context's caller holds. all means the
// exclusive global authority, which covers every item.
type heldImportTarget struct {
	all  bool
	item int64
}

func (h heldImportTarget) covers(item int64) bool { return h.all || h.item == item }

func heldTarget(ctx context.Context) (heldImportTarget, bool) {
	h, ok := ctx.Value(targetHeldKey{}).(heldImportTarget)
	return h, ok
}

// ErrImportTargetsBusy reports that the whole-library authority could not be
// taken within the wait because titles are being published.
var ErrImportTargetsBusy = errors.New("imports are publishing to the library; try again shortly")

// lockImportTarget takes the publication authority for one media item and
// returns the context that records it.
func (s *Service) lockImportTarget(ctx context.Context, item int64) (context.Context, func()) {
	if h, ok := heldTarget(ctx); ok && h.covers(item) {
		return ctx, func() {}
	}
	if item < 0 {
		item = 0
	}
	t := &s.importTargets
	t.all.RLock()
	t.mu.Lock()
	if t.items == nil {
		t.items = map[int64]*importTargetLock{}
	}
	l := t.items[item]
	if l == nil {
		l = &importTargetLock{}
		t.items[item] = l
	}
	l.refs++
	t.mu.Unlock()
	l.mu.Lock()
	var once sync.Once
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{item: item}), func() {
		once.Do(func() {
			l.mu.Unlock()
			t.mu.Lock()
			if l.refs--; l.refs == 0 {
				delete(t.items, item)
			}
			t.mu.Unlock()
			t.all.RUnlock()
		})
	}
}

// lockAllImportTargets takes the exclusive global publication authority,
// waiting at most wait (0: until ctx ends) for every held title to finish.
// It never queues as a writer, so it never holds up other titles while it
// waits.
func (s *Service) lockAllImportTargets(ctx context.Context, wait time.Duration) (context.Context, func(), error) {
	if h, ok := heldTarget(ctx); ok {
		if h.all {
			return ctx, func() {}, nil
		}
		return ctx, func() {}, fmt.Errorf("whole-library authority requested while holding title %d", h.item)
	}
	waitCtx := ctx
	if wait > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	backoff := 10 * time.Millisecond
	for !s.importTargets.all.TryLock() {
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx, func() {}, ctx.Err()
			}
			return ctx, func() {}, ErrImportTargetsBusy
		case <-time.After(backoff):
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
	var once sync.Once
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{all: true}), func() {
		once.Do(s.importTargets.all.Unlock)
	}, nil
}
