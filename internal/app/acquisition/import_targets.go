package acquisition

import (
	"context"
	"sync"
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
//     work on the same item is still serialized exactly as before.
//   - lockAllImportTargets() holds the global authority exclusively, for work
//     that is not scoped to one item: operator deletes addressed by
//     completed-folder path, and crash reconciliation of the placement
//     journal at start.
//
// Neither is reentrant, and no path takes two item locks: a holder passes
// the scope it holds down through targetHeldKey, and nested work checks
// that scope instead of locking again. The storage-decision lock is still
// the single serializer for the short final publish-and-commit window.
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

func (h heldImportTarget) covers(item int64) bool { return h.all || (item > 0 && h.item == item) }

func heldTarget(ctx context.Context) (heldImportTarget, bool) {
	h, ok := ctx.Value(targetHeldKey{}).(heldImportTarget)
	return h, ok
}

// lockImportTarget takes the publication authority for one media item and
// returns the context that records it. An unknown item (0) takes the global
// authority: there is no narrower scope to prove.
func (s *Service) lockImportTarget(ctx context.Context, item int64) (context.Context, func()) {
	if h, ok := heldTarget(ctx); ok && h.covers(item) {
		return ctx, func() {}
	}
	if item <= 0 {
		return s.lockAllImportTargets(ctx)
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
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{item: item}), func() {
		l.mu.Unlock()
		t.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(t.items, item)
		}
		t.mu.Unlock()
		t.all.RUnlock()
	}
}

// lockAllImportTargets takes the exclusive global publication authority.
func (s *Service) lockAllImportTargets(ctx context.Context) (context.Context, func()) {
	if h, ok := heldTarget(ctx); ok && h.all {
		return ctx, func() {}
	}
	s.importTargets.all.Lock()
	return context.WithValue(ctx, targetHeldKey{}, heldImportTarget{all: true}), s.importTargets.all.Unlock
}
