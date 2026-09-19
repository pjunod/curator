package openlibrary

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed *int
}

func (b trackedBody) Close() error {
	*b.closed++
	return nil
}

func TestGetRetriesTransientFailures(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
	for _, tc := range []struct {
		name       string
		status     int
		requestErr error
		readErr    error
		retryAfter string
		persistent bool
		wantCalls  int
		wantWait   time.Duration
		wantErr    bool
	}{
		{name: "connection reset", requestErr: reset, wantCalls: 2, wantWait: time.Second},
		{name: "closed connection", requestErr: io.EOF, wantCalls: 2, wantWait: time.Second},
		{name: "client timeout", requestErr: context.DeadlineExceeded, wantCalls: 2, wantWait: time.Second},
		{name: "interrupted body", readErr: io.ErrUnexpectedEOF, wantCalls: 2, wantWait: time.Second},
		{name: "rate limited", status: 429, retryAfter: "5", wantCalls: 2, wantWait: 5 * time.Second},
		{name: "internal error", status: 500, wantCalls: 2, wantWait: time.Second},
		{name: "bad gateway", status: 502, wantCalls: 2, wantWait: time.Second},
		{name: "unavailable", status: 503, wantCalls: 2, wantWait: time.Second},
		{name: "gateway timeout", status: 504, wantCalls: 2, wantWait: time.Second},
		{name: "persistent reset", requestErr: reset, persistent: true, wantCalls: 3, wantWait: 3 * time.Second, wantErr: true},
		{name: "persistent unavailable", status: 503, persistent: true, wantCalls: 3, wantWait: 3 * time.Second, wantErr: true},
		{name: "long retry after", status: 429, retryAfter: "120", wantCalls: 1, wantErr: true},
		{name: "not found", status: 404, wantCalls: 1, wantErr: true},
		{name: "forbidden", status: 403, wantCalls: 1, wantErr: true},
		{name: "permanent request error", requestErr: errors.New("bad configuration"), wantCalls: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls, bodies, closed := 0, 0, 0
				c := New("https://example.test")
				c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Path != "/works/OL3169376W.json" || r.Header.Get("User-Agent") != userAgent || r.Header.Get("Accept") != "application/json" {
						t.Fatalf("unexpected request: %v, headers: %v", r.URL, r.Header)
					}
					fail := calls == 1 || tc.persistent
					if fail && tc.requestErr != nil {
						return nil, tc.requestErr
					}
					status := http.StatusOK
					var reader io.Reader = strings.NewReader(`{"title":"The hitchhiker's guide to calculus"}`)
					header := make(http.Header)
					if fail {
						if tc.status != 0 {
							status = tc.status
						}
						if tc.readErr != nil {
							reader = iotest.ErrReader(tc.readErr)
						}
						header.Set("Retry-After", tc.retryAfter)
					}
					bodies++
					return &http.Response{StatusCode: status, Header: header, Body: trackedBody{reader, &closed}}, nil
				})
				start := time.Now()
				var work workResp
				err := c.get(context.Background(), "/works/OL3169376W.json", nil, &work)
				if (err != nil) != tc.wantErr {
					t.Fatalf("error = %v, want error = %v", err, tc.wantErr)
				}
				if tc.persistent && tc.requestErr != nil && !errors.Is(err, tc.requestErr) {
					t.Fatalf("original error lost: %v", err)
				}
				if calls != tc.wantCalls || time.Since(start) != tc.wantWait || closed != bodies {
					t.Fatalf("calls=%d wait=%v closed=%d/%d; want calls=%d wait=%v", calls, time.Since(start), closed, bodies, tc.wantCalls, tc.wantWait)
				}
				if tc.wantErr {
					if len(c.cache) != 0 {
						t.Fatal("failed request was cached")
					}
					return
				}
				if work.Title != "The hitchhiker's guide to calculus" {
					t.Fatalf("work = %+v", work)
				}
				if err := c.get(context.Background(), "/works/OL3169376W.json", nil, &work); err != nil || calls != tc.wantCalls {
					t.Fatalf("successful response was not cached: calls=%d err=%v", calls, err)
				}
			})
		})
	}
}

func TestGetCanceledDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := New("https://example.test")
		c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, syscall.ECONNRESET
		})
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		start := time.Now()
		var out workResp
		err := c.get(ctx, "/work.json", nil, &out)
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || time.Since(start) != 500*time.Millisecond {
			t.Fatalf("calls=%d elapsed=%v error=%v", calls, time.Since(start), err)
		}
	})
}

func TestGetRateLimitAndInvalidJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New("https://example.test")
		calls := 0
		c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			body := `{"title":`
			if calls > 1 {
				body = `{"title":"Recovered"}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		start := time.Now()
		var out workResp
		if err := c.get(context.Background(), "/work.json", nil, &out); err == nil || calls != 1 {
			t.Fatalf("invalid JSON: calls=%d err=%v", calls, err)
		}
		if err := c.get(context.Background(), "/work.json", nil, &out); err != nil || out.Title != "Recovered" {
			t.Fatalf("invalid JSON was cached: out=%+v err=%v", out, err)
		}
		if time.Since(start) != time.Second {
			t.Fatalf("requests were not paced at 1/s: %v", time.Since(start))
		}
	})
}

func TestRetryDelay(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"", time.Second}, {"invalid", time.Second}, {"-1", time.Second},
		{"0", time.Second}, {"5", 5 * time.Second},
		{"9223372036854775807", maxRetryWait + time.Second},
		{now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second},
		{now.Add(-10 * time.Second).Format(http.TimeFormat), time.Second},
		{now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
	} {
		if got := retryDelay(tc.header, time.Second, now); got != tc.want {
			t.Errorf("retryDelay(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
