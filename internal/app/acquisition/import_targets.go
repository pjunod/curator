package acquisition

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
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
//     completed-folder path and crash reconciliation of the placement journal
//     at start. Recovery is scoped to its title: its preview is leased against
//     that title's revision (lifecycle_item_revision), not the library's.
//
// Exclusive acquisition never queues. sync.RWMutex makes new readers wait
// behind a waiting writer, so one queued exclusive request would stop every
// title behind the slowest placement in flight — the original failure.
// lockAllImportTargets instead polls TryLock until no title is held, within
// a bounded wait, and reports busy rather than hanging.
//
// Neither lock is reentrant. A holder passes the scope it holds down through
// targetHeldKey; nested work on a covered scope does not lock again, and
// asking for the global authority while holding a title is refused. A path
// takes two item locks only through lockImportTargets (Re-match, ADR 0027):
// sorted by id, each within a bounded wait, all or none. The storage-decision lock is still the single
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
	all   bool
	items []int64
	// active ends at release, so a context that outlives its lock can never
	// be mistaken for a holder (and skip locking).
	active *atomic.Bool
}

func (h heldImportTarget) covers(item int64) bool {
	if h.all {
		return true
	}
	for _, id := range h.items {
		if id == item {
			return true
		}
	}
	return false
}

func heldTarget(ctx context.Context) (heldImportTarget, bool) {
	h, ok := ctx.Value(targetHeldKey{}).(heldImportTarget)
	return h, ok && h.active != nil && h.active.Load()
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
	active := &atomic.Bool{}
	active.Store(true)
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{items: []int64{item}, active: active}), func() {
		once.Do(func() {
			active.Store(false)
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
		return ctx, func() {}, fmt.Errorf("whole-library authority requested while holding title %v", h.items)
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
	active := &atomic.Bool{}
	active.Store(true)
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{all: true, active: active}), func() {
		once.Do(func() {
			active.Store(false)
			s.importTargets.all.Unlock()
		})
	}, nil
}

// ErrImportTargetBusy reports that a title's authority could not be taken
// within the wait because that title is being published.
var ErrImportTargetBusy = errors.New("this title is being imported; try again shortly")

// waitImportTarget takes one item's authority, waiting at most wait for the
// title's work in flight. Like lockAllImportTargets it never parks behind the
// global authority as a waiting reader: it polls, and reports busy.
func (s *Service) waitImportTarget(ctx context.Context, item int64, wait time.Duration) (context.Context, func(), error) {
	if h, ok := heldTarget(ctx); ok && !h.covers(item) {
		return ctx, func() {}, fmt.Errorf("title %d authority requested while holding title %v", item, h.items)
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	backoff := 10 * time.Millisecond
	for {
		locked, release, ok := s.tryLockImportTarget(ctx, item)
		if ok {
			return locked, release, nil
		}
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return ctx, func() {}, ctx.Err()
			}
			return ctx, func() {}, ErrImportTargetBusy
		case <-time.After(backoff):
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
}

// tryLockImportTarget is for short operator decisions that already hold the
// completed-inventory lock. Busy publication must not pin that global lock.
func (s *Service) tryLockImportTarget(ctx context.Context, item int64) (context.Context, func(), bool) {
	if h, ok := heldTarget(ctx); ok {
		if h.covers(item) {
			return ctx, func() {}, true
		}
		return ctx, func() {}, false
	}
	if item < 0 {
		item = 0
	}
	t := &s.importTargets
	if !t.all.TryRLock() {
		return ctx, func() {}, false
	}
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
	releaseRef := func() {
		t.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(t.items, item)
		}
		t.mu.Unlock()
		t.all.RUnlock()
	}
	if !l.mu.TryLock() {
		releaseRef()
		return ctx, func() {}, false
	}
	var once sync.Once
	active := &atomic.Bool{}
	active.Store(true)
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{items: []int64{item}, active: active}), func() {
		once.Do(func() { active.Store(false); l.mu.Unlock(); releaseRef() })
	}, true
}

// lockImportTargets takes the authority for several titles at once: in
// ascending id order, each within wait, and all or none. It is the only way
// to hold two titles (Re-match moves files between them), and the sorted
// order is what keeps two such holders from deadlocking each other.
func (s *Service) lockImportTargets(ctx context.Context, wait time.Duration, ids ...int64) (context.Context, func(), error) {
	if _, ok := heldTarget(ctx); ok {
		return ctx, func() {}, fmt.Errorf("several titles' authority requested while holding one")
	}
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var held []int64
	var releases []func()
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for i, id := range sorted {
		if i > 0 && id == sorted[i-1] {
			continue
		}
		_, release, err := s.waitImportTarget(ctx, id, wait)
		if err != nil {
			releaseAll()
			return ctx, func() {}, err
		}
		held, releases = append(held, id), append(releases, release)
	}
	active := &atomic.Bool{}
	active.Store(true)
	var once sync.Once
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{items: held, active: active}), func() {
		once.Do(func() { active.Store(false); releaseAll() })
	}, nil
}
