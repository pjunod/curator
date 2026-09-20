package acquisition

import (
	"context"
	"fmt"
	"sync"

	"github.com/pjunod/monarr/internal/domain"
)

// acquisitionReservations serializes unattended acquisition for one library
// item and copy inside this process. The deliberately conservative key makes
// an episode conflict with a season pack for the same copy while still
// allowing independent copies to make progress.
type acquisitionReservations struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func reservationKey(w domain.Wantable) string {
	return fmt.Sprintf("%d:c%d", w.MediaItemID(), domain.WantableCopy(w))
}

func (r *acquisitionReservations) acquire(ctx context.Context, w domain.Wantable) (func(), error) {
	r.mu.Lock()
	if r.locks == nil {
		r.locks = map[string]chan struct{}{}
	}
	key := reservationKey(w)
	lock := r.locks[key]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		r.locks[key] = lock
	}
	r.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock:
		return func() { lock <- struct{}{} }, nil
	}
}
