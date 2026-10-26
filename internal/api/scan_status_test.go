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

type scanQueueFunc func(context.Context, domain.Job) (bool, error)

func (f scanQueueFunc) EnqueueUnique(ctx context.Context, job domain.Job) (bool, error) {
	return f(ctx, job)
}

// A status read failure must remain an error, never a synthetic idle or
// completed state that could make a broken scan look successful.
func TestScanStatusStorageFailureIsVisible(t *testing.T) {
	e := newAPIEnv(t)
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	rr := do(t, e.h, http.MethodGet, "/api/v1/library/scan", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("unavailable status: %d %s", rr.Code, rr.Body.String())
	}
	var body struct{ Message string }
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Message == "" {
		t.Fatalf("missing actionable status error: %s, %v", rr.Body.String(), err)
	}
}

func TestScanAcceptedButStatusReadFails(t *testing.T) {
	e := newAPIEnv(t)
	e.lib.WithQueue(scanQueueFunc(func(ctx context.Context, job domain.Job) (bool, error) {
		if _, err := e.db.EnqueueJob(ctx, job); err != nil {
			return false, err
		}
		// The job is durable, but the status store goes away before the
		// response can be built. Polling can recover after storage returns.
		return true, e.db.Close()
	}))
	rr := do(t, e.h, http.MethodPost, "/api/v1/library/scan", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("unavailable accepted-job status: %d %s", rr.Code, rr.Body.String())
	}
	var body struct{ Message string }
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Message == "" {
		t.Fatalf("missing accepted-job status error: %s, %v", rr.Body.String(), err)
	}
}

func TestCorruptScanReportDoesNotBecomeSuccessOrEmptyResults(t *testing.T) {
	e := newAPIEnv(t)
	if err := e.db.SetMeta(context.Background(), "last_scan_report", "{"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/library/scan/report", "/api/v1/library/scan"} {
		rr := do(t, e.h, http.MethodGet, path, "")
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("corrupt report at %s: %d %s", path, rr.Code, rr.Body.String())
		}
		var body struct{ Message string }
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Message == "" {
			t.Fatalf("missing corrupt-report error: %s, %v", rr.Body.String(), err)
		}
	}
}
