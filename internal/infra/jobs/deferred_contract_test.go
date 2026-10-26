package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

type contractDeferral struct{ until time.Time }

func (e contractDeferral) Error() string            { return "provider quota deferred" }
func (e contractDeferral) DeferredUntil() time.Time { return e.until }

type contractDeferredStore struct {
	*fakeStore
	deadline              time.Time
	cause                 string
	ctxErr, errorToReturn error
}

func (s *contractDeferredStore) DeferJob(ctx context.Context, _ int64, cause string, at time.Time) error {
	s.deadline = at
	s.cause = cause
	s.ctxErr = ctx.Err()
	return s.errorToReturn
}

func TestDeferredJobsPreserveProviderDeadlineWithoutNormalRetry(t *testing.T) {
	until := time.Now().UTC().Add(time.Hour)
	for _, failedSave := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "save failure"}[failedSave], func(t *testing.T) {
			base := newFakeStore()
			store := &contractDeferredStore{fakeStore: base}
			if failedSave {
				store.errorToReturn = errors.New("storage unavailable")
			}
			var log bytes.Buffer
			q := New(store, nil, slog.New(slog.NewTextHandler(&log, nil)), Options{})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			q.finish(ctx, domain.Job{ID: 7, Kind: "library.adopt", Attempts: 1, MaxAttempts: 3}, contractDeferral{until})
			if !store.deadline.Equal(until) || store.cause != "provider quota deferred" || store.ctxErr != nil {
				t.Fatalf("deferral lost deadline or used cancelled context: %+v", store)
			}
			for _, action := range []string{"complete", "retry", "fail"} {
				if base.count(action) != 0 {
					t.Fatalf("deferred work entered %s", action)
				}
			}
			if failedSave && !strings.Contains(log.String(), "defer failed") {
				t.Fatalf("deferral persistence error was silent: %s", log.String())
			}
		})
	}
	base := newFakeStore()
	var log bytes.Buffer
	q := New(base, nil, slog.New(slog.NewTextHandler(&log, nil)), Options{})
	q.finish(context.Background(), domain.Job{ID: 7}, contractDeferral{until})
	if !strings.Contains(log.String(), "lacks deferred scheduling") || base.count("retry") != 0 || base.count("fail") != 0 || base.count("complete") != 0 {
		t.Fatalf("unsupported deferral was silently retired: %s", log.String())
	}
}
