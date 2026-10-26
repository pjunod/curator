package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/jobs"
)

func TestScanStatusTracksDurableWork(t *testing.T) {
	e := newAPIEnv(t)
	e.lib.WithQueue(jobs.New(e.db, e.bus, nil, jobs.Options{}))
	ctx := context.Background()
	read := func(method, state string) library.ScanStatus {
		t.Helper()
		rr := do(t, e.h, method, "/api/v1/library/scan", "")
		want := http.StatusOK
		if method == http.MethodPost {
			want = http.StatusAccepted
		}
		var got library.ScanStatus
		if rr.Code != want || json.Unmarshal(rr.Body.Bytes(), &got) != nil || got.State != state {
			t.Fatalf("%s scan: %d %s; want %s", method, rr.Code, rr.Body.String(), state)
		}
		return got
	}
	read(http.MethodGet, "idle")
	first := read(http.MethodPost, "queued")
	if first.RequestedAt == nil || first.RequestedAt.IsZero() || first.ID == 0 || read(http.MethodPost, "queued").ID != first.ID {
		t.Fatal("duplicate scan request must reuse the active job")
	}
	// A newer, unrelated job must not masquerade as scan progress.
	if _, err := e.db.EnqueueJob(ctx, domain.Job{Kind: library.JobProbe}); err != nil {
		t.Fatal(err)
	}
	claimed, err := e.db.ClaimJob(ctx, "scan-test", nil, time.Minute)
	if err != nil || claimed.ID != first.ID {
		t.Fatalf("claim scan: %+v, %v", claimed, err)
	}
	read(http.MethodGet, "running")
	if err := e.db.RetryJob(ctx, first.ID, "disk unavailable", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	retry := read(http.MethodGet, "queued")
	if retry.Error != "disk unavailable" || retry.Attempts != 1 {
		t.Fatalf("retry details missing: %+v", retry)
	}
	if err := e.db.FailJob(ctx, first.ID, "disk unavailable"); err != nil {
		t.Fatal(err)
	}
	if read(http.MethodGet, "failed").Error != "disk unavailable" {
		t.Fatal("failure must reach the caller")
	}
	next := read(http.MethodPost, "queued")
	if next.ID == first.ID {
		t.Fatal("a failed scan must permit a fresh request")
	}
	if err := e.db.CompleteJob(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	read(http.MethodGet, "completed")
}

type unavailableScanQueue struct{}

func (unavailableScanQueue) EnqueueUnique(context.Context, domain.Job) (bool, error) {
	return false, errors.New("queue unavailable")
}

func TestScanEnqueueFailureReachesCaller(t *testing.T) {
	e := newAPIEnv(t)
	e.lib.WithQueue(unavailableScanQueue{})
	rr := do(t, e.h, http.MethodPost, "/api/v1/library/scan", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("enqueue failure: %d %s", rr.Code, rr.Body.String())
	}
}
