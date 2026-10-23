package acquisition

import (
	"context"
	"sync/atomic"
)

type storageDecisionKey struct{}
type storageDecisionLease struct {
	owner  *Service
	active atomic.Bool
}

// Lock order is download row -> import target -> storage decision. The target
// reservation may cover large import staging, but storage authority is held
// only for final identity checks and mutation. Context nesting avoids recursive
// locking inside that short section; the lease becomes inactive on release so
// a context reused later cannot accidentally claim it still owns the mutex.
func (s *Service) lockStorageDecision(ctx context.Context) (context.Context, func()) {
	if lease, ok := ctx.Value(storageDecisionKey{}).(*storageDecisionLease); ok && lease != nil && lease.owner == s && lease.active.Load() {
		return ctx, func() {}
	}
	s.storageDecisionMu.Lock()
	lease := &storageDecisionLease{owner: s}
	lease.active.Store(true)
	locked := context.WithValue(ctx, storageDecisionKey{}, lease)
	return locked, func() {
		lease.active.Store(false)
		s.storageDecisionMu.Unlock()
	}
}
