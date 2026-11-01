package jobs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

type blockedHeartbeatStore struct {
	*fakeStore
	entered    chan struct{}
	cancelled  chan struct{}
	release    chan struct{}
	entryOnce  sync.Once
	cancelOnce sync.Once
}

func (s *blockedHeartbeatStore) HeartbeatJob(ctx context.Context, _ int64, _ string, _ time.Duration) (bool, error) {
	s.entryOnce.Do(func() { close(s.entered) })
	<-ctx.Done()
	s.cancelOnce.Do(func() { close(s.cancelled) })
	<-s.release // A store may still be unwinding after cancellation.
	return false, ctx.Err()
}

func TestShutdownJoinsLeaseHeartbeatBeforeQueueReturns(t *testing.T) {
	store := &blockedHeartbeatStore{
		fakeStore: newFakeStore(domain.Job{ID: 1, Kind: "shutdown", Attempts: 1, MaxAttempts: 3}),
		entered:   make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{}),
	}
	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	handlerReturned := make(chan struct{})
	if err := q.Register("shutdown", func(ctx context.Context, _ domain.Job) error {
		<-ctx.Done()
		close(handlerReturned)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	joined := make(chan struct{})
	go func() { q.Wait(); close(joined) }()
	t.Cleanup(func() {
		cancel()
		release()
		awaitShutdown(t, joined, "queue cleanup")
	})
	awaitShutdown(t, store.entered, "heartbeat entry")
	cancel()
	awaitShutdown(t, handlerReturned, "handler return")
	awaitShutdown(t, store.cancelled, "heartbeat cancellation")
	select {
	case <-joined:
		t.Fatal("queue returned while heartbeat still owned a store call")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	awaitShutdown(t, joined, "heartbeat and queue completion")
}

func awaitShutdown(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for " + what)
	}
}
