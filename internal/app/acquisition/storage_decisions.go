package acquisition

import "context"

type storageDecisionKey struct{}

// Lock order is download row -> storage decision -> import placement. Normal
// admission releases this mutex as soon as its durable active row is visible;
// copies still run on the independent importer workers. Inline test/legacy
// paths reuse the decision context rather than recursively locking the mutex.
func (s *Service) lockStorageDecision(ctx context.Context) (context.Context, func()) {
	if ctx.Value(storageDecisionKey{}) == s {
		return ctx, func() {}
	}
	s.storageDecisionMu.Lock()
	return context.WithValue(ctx, storageDecisionKey{}, s), s.storageDecisionMu.Unlock
}
