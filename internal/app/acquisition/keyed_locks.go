package acquisition

import (
	"context"
	"sync"
)

// keyedLocks keeps the release notification only while a key is held.
type keyedLocks struct {
	held sync.Map
}

func (l *keyedLocks) lock(ctx context.Context, id int64) (func(), error) {
	released := make(chan struct{})
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, held := l.held.LoadOrStore(id, released)
		if !held {
			return func() {
				l.held.Delete(id)
				close(released)
			}, nil
		}
		select {
		case <-current.(chan struct{}):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
