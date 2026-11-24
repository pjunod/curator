package nzbd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestAddTaggedUsesTheNativeTransferAndReleaseFields(t *testing.T) {
	var rec recorder
	srv := fake(t, &rec, nil)
	handle, err := client(t, srv, "", "token").AddTagged(
		t.Context(), "https://indexer.test/release.nzb", "movies", "Release.Name", "transfer-opaque_123",
	)
	if err != nil || handle != "1" {
		t.Fatalf("AddTagged = %q, %v", handle, err)
	}
	if rec.method != http.MethodPost || rec.path != "/api/v1/jobs" || rec.query.Get("name") != "Release.Name" || rec.query.Get("category") != "movies" {
		t.Fatalf("AddTagged request = %s %s?%s", rec.method, rec.path, rec.query.Encode())
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(rec.query.Get("params")), &params); err != nil {
		t.Fatalf("params %q: %v", rec.query.Get("params"), err)
	}
	if params[TransferParam] != "transfer-opaque_123" || len(params) != 1 {
		t.Fatalf("AddTagged params = %#v, want exactly the opaque transfer token", params)
	}
}

func TestCapacityReportsIndependentRunnerBlockers(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.38.0","disk_low":true,"quota_reached":true,"blocked_servers":[1,2],"health_abort":true,"download_paused":true}`))
	}))
	defer srv.Close()
	got, err := New(ports.ClientConfig{URL: srv.URL}).Capacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/v1/status" {
		t.Fatalf("capacity request = %s %s", method, path)
	}
	if got.Version != "0.38.0" || !got.DiskLow || !got.QuotaReached || got.BlockedServers != 2 || !got.HealthAbort || !got.Paused {
		t.Fatalf("capacity = %+v", got)
	}
}

func TestResumeTargetsOnlyTheExactRetainedJob(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantErr    string
		wantStatus int
	}{
		{name: "resumed", status: http.StatusOK, body: `{"ok":true}`},
		{name: "custody hold", status: http.StatusOK, body: `{"ok":false}`, wantErr: "custody hold"},
		{name: "conflict", status: http.StatusConflict, body: `{"error":"job is no longer resumable"}`, wantErr: "job is no longer resumable", wantStatus: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var method, path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.EscapedPath()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client := New(ports.ClientConfig{URL: srv.URL})
			err := client.Resume(t.Context(), ports.Handle("job/17"))
			if method != http.MethodPost || path != "/api/v1/jobs/job%2F17/actions/resume" {
				t.Fatalf("resume request = %s %s", method, path)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Resume error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Resume error = %v, want %q", err, tc.wantErr)
			}
			if tc.wantStatus != 0 {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.HTTPStatusCode() != tc.wantStatus {
					t.Fatalf("Resume error = %T %v, want HTTP %d", err, err, tc.wantStatus)
				}
			}
		})
	}
}

func TestRequestAdmissionRejectsWrongIdentityAndPreservesRetryBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want string
	}{
		{name: "wrong token", code: http.StatusOK, body: `{"protocolVersion":1,"requestToken":"other","jobId":3,"generation":"g","runnerInstanceId":"r","outcome":"failed"}`, want: "identity or protocol version mismatch"},
		{name: "wrong protocol", code: http.StatusOK, body: `{"protocolVersion":2,"requestToken":"t-3-123abc","jobId":3,"generation":"g","runnerInstanceId":"r","outcome":"failed"}`, want: "identity or protocol version mismatch"},
		{name: "temporary outage", code: http.StatusServiceUnavailable, body: `{"error":"admission journal is recovering"}`, want: "admission journal is recovering"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var escapedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				escapedPath = r.URL.EscapedPath()
				if tc.code == http.StatusServiceUnavailable {
					w.Header().Set("Retry-After", "30")
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client := New(ports.ClientConfig{URL: srv.URL})
			_, found, err := client.GetRequestAdmission(t.Context(), "t-3-123abc")
			if found || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("lookup found=%v err=%v; want error containing %q", found, err, tc.want)
			}
			if escapedPath != "/api/v1/request-admissions/t-3-123abc" {
				t.Fatalf("admission lookup path = %q", escapedPath)
			}
			if tc.code == http.StatusServiceUnavailable {
				var retry ports.RetryDeadline
				if !errors.As(err, &retry) || retry.RetryDeadline().Before(time.Now().Add(25*time.Second)) {
					t.Fatalf("retry outage lost Retry-After boundary: %T %v", err, err)
				}
			}
		})
	}
	if _, _, err := New(ports.ClientConfig{URL: "http://runner.invalid"}).GetRequestAdmission(t.Context(), " \t"); err == nil {
		t.Fatal("blank transfer token was accepted")
	}
}

// A zero-article health failure must not spend a full release's bandwidth.
// Job IDs alone are reusable; only the same transfer can supply measurements.
func TestFailedReceiptSupplementsFetchedCountsFromMatchingHistory(t *testing.T) {
	for _, tc := range []struct {
		name, row string
		want      int64
	}{
		{"matching", `{"job":3,"params":[["monarr-transfer","t-3"]],"record":{"total_articles":100,"success_articles":0}}`, 100},
		{"foreign token", `{"job":3,"params":[["monarr-transfer","other"]],"record":{"total_articles":100,"success_articles":0}}`, 0},
		{"foreign job", `{"job":4,"params":[["monarr-transfer","t-3"]],"record":{"total_articles":100,"success_articles":0}}`, 0},
		{"unmeasured", `{"job":3,"params":[["monarr-transfer","t-3"]],"record":{}}`, 0},
		{"unavailable", ``, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/request-admissions/t-3" {
					_, _ = w.Write([]byte(`{"protocolVersion":1,"requestToken":"t-3","jobId":3,"outcome":"failed","failureReason":"health","blamesRelease":true}`))
					return
				}
				if r.URL.Path != "/api/v1/history" || r.URL.Query().Get("limit") != "100" {
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
					return
				}
				if tc.row == "" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"entries":[` + tc.row + `]}`))
			}))
			defer srv.Close()
			got, found, err := New(ports.ClientConfig{URL: srv.URL}).GetRequestAdmission(t.Context(), "t-3")
			if err != nil || !found || got.Fetched.TotalArticles != tc.want || got.Fetched.SuccessArticles != 0 || got.FailureReason != "health" || got.BlamesRelease == nil || !*got.BlamesRelease {
				t.Fatalf("receipt=%+v found=%v err=%v", got, found, err)
			}
		})
	}
}
