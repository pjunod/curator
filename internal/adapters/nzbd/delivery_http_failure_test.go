package nzbd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryHTTPFailuresPreserveStatusAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		retryAfter string
		message    string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"bad token"}`, message: "authentication rejected"},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"Runner is busy"}`, retryAfter: "45", message: "Runner is busy"},
		{name: "plain server failure", status: http.StatusServiceUnavailable, body: "maintenance", message: "unexpected delivery response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/deliveries" {
					t.Errorf("request = %s %s", r.Method, r.URL)
				}
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := New(ports.ClientConfig{URL: srv.URL}).ListDeliveries(t.Context(), 0, 5)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %T %v, want APIError", err, err)
			}
			if apiErr.HTTPStatusCode() != tc.status || !strings.Contains(apiErr.Error(), tc.message) {
				t.Fatalf("APIError = %+v, want status %d and message %q", apiErr, tc.status, tc.message)
			}
			if tc.retryAfter != "" {
				if deadline := apiErr.RetryDeadline(); deadline.Before(time.Now().Add(40 * time.Second)) {
					t.Fatalf("Retry-After deadline = %v, want at least 40s ahead", deadline)
				}
			} else if !apiErr.RetryDeadline().IsZero() {
				t.Fatalf("unexpected retry deadline: %v", apiErr.RetryDeadline())
			}
		})
	}
}

func TestDeliveryClientCancellationReturnsContextError(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := New(ports.ClientConfig{URL: srv.URL}).ListDeliveries(ctx, 0, 5)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive delivery request")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("delivery request did not return after cancellation")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type controlledResponseBody struct {
	reader   io.Reader
	readErr  error
	closeErr error
	closed   atomic.Bool
}

func (b *controlledResponseBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	if b.reader == nil {
		return 0, io.EOF
	}
	return b.reader.Read(p)
}

func (b *controlledResponseBody) Close() error {
	b.closed.Store(true)
	return b.closeErr
}

func TestDeliveryResponseReadCloseAndSizeFailuresAreReturned(t *testing.T) {
	readFailure := errors.New("injected response read failure")
	closeFailure := errors.New("injected response close failure")
	for _, tc := range []struct {
		name      string
		body      *controlledResponseBody
		want      string
		wantCause error
	}{
		{name: "read failure", body: &controlledResponseBody{readErr: readFailure}, want: "read delivery response", wantCause: readFailure},
		{name: "close failure", body: &controlledResponseBody{reader: strings.NewReader("{}"), closeErr: closeFailure}, want: "close delivery response", wantCause: closeFailure},
		{name: "oversized response", body: &controlledResponseBody{reader: strings.NewReader(strings.Repeat("x", (8<<20)+1))}, want: "exceeds 8 MiB"},
		{name: "invalid JSON", body: &controlledResponseBody{reader: strings.NewReader("{")}, want: "bad delivery response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := New(ports.ClientConfig{URL: "http://runner.invalid"})
			client.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: tc.body}, nil
			})}
			_, err := client.ListDeliveries(t.Context(), 0, 5)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
			if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
				t.Fatalf("error %v does not wrap injected cause %v", err, tc.wantCause)
			}
			if !tc.body.closed.Load() {
				t.Fatal("response body was not closed after read/parse failure")
			}
		})
	}
}

func TestDeliveryListRejectsIncompleteOrMalformedPages(t *testing.T) {
	valid := `{"protocolVersion":1,"sequence":1,"deliveryId":"d1","publicationGeneration":"g1","artifactId":"a1","jobId":1,"requestToken":"t-1","runnerInstanceId":"r1","publicationPath":"/completed/r","manifestDigest":"digest","files":[{"path":"movie.mkv","sizeBytes":1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}],"readyAt":1}`
	for _, tc := range []struct {
		name  string
		body  string
		limit int
		want  string
	}{
		{name: "no delivery identity", body: `{"protocolVersion":1,"deliveries":[{"protocolVersion":1,"deliveryId":"","publicationGeneration":"g1","files":[{"path":"x","sizeBytes":1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}],"readyAt":1}]}`, limit: 1, want: "size limits"},
		{name: "negative file size", body: `{"protocolVersion":1,"deliveries":[{"protocolVersion":1,"deliveryId":"d1","publicationGeneration":"g1","files":[{"path":"x","sizeBytes":-1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}],"readyAt":1}]}`, limit: 1, want: "malformed file entry"},
		{name: "invalid ready timestamp", body: `{"protocolVersion":1,"deliveries":[{"protocolVersion":1,"deliveryId":"d1","publicationGeneration":"g1","files":[{"path":"x","sizeBytes":1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}],"readyAt":1e9999}]}`, limit: 1, want: "malformed readyAt"},
		{name: "server exceeds requested page", body: `{"protocolVersion":1,"deliveries":[` + valid + `,` + valid + `],"nextCursor":2}`, limit: 1, want: "returned 2 rows for limit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := New(ports.ClientConfig{URL: srv.URL}).ListDeliveries(t.Context(), 0, tc.limit)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestDeliveryMethodsRejectIncompleteRequestsBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	client := New(ports.ClientConfig{URL: srv.URL})

	for _, acceptance := range []ports.RunnerDeliveryAcceptance{
		{ProtocolVersion: 2, PublicationGeneration: "g", ManifestDigest: "d", ConsumerReceiptID: "r"},
		{ProtocolVersion: 1, PublicationGeneration: "", ManifestDigest: "d", ConsumerReceiptID: "r"},
		{ProtocolVersion: 1, PublicationGeneration: "g", ManifestDigest: "", ConsumerReceiptID: "r"},
		{ProtocolVersion: 1, PublicationGeneration: "g", ManifestDigest: "d", ConsumerReceiptID: ""},
	} {
		if err := client.AcceptDelivery(t.Context(), "d1", acceptance); err == nil {
			t.Errorf("incomplete acceptance %+v was sent", acceptance)
		}
	}
	complete := ports.RunnerDeliveryAcceptance{ProtocolVersion: 1, PublicationGeneration: "g", ManifestDigest: "d", ConsumerReceiptID: "r"}
	if err := client.AcceptDelivery(t.Context(), "", complete); err == nil {
		t.Error("empty delivery ID was sent")
	}
	if _, err := client.ListDeliveries(t.Context(), 0, 0); err == nil {
		t.Error("zero page limit was sent")
	}
	if _, err := client.GetDelivery(t.Context(), strings.Repeat("d", maxDeliveryIdentityBytes+1)); err == nil {
		t.Error("oversized delivery ID was sent")
	}
	if _, err := client.RetryDeliverySourceCleanup(t.Context(), ""); err == nil {
		t.Error("empty delivery ID cleanup retry was sent")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid requests reached Runner %d times", got)
	}
}

func TestDeliveryAcceptanceOutageRetainsRetryAndRequestIdentity(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		var got struct {
			ProtocolVersion       int    `json:"protocolVersion"`
			PublicationGeneration string `json:"publicationGeneration"`
			ManifestDigest        string `json:"manifestDigest"`
			ConsumerReceiptID     string `json:"consumerReceiptId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("acceptance JSON: %v", err)
		}
		if got.ProtocolVersion != 1 || got.PublicationGeneration != "pub-generation" || got.ManifestDigest != "manifest-digest" || got.ConsumerReceiptID != "receipt-id" {
			t.Errorf("acceptance request body = %+v", got)
		}
		w.Header().Set("Retry-After", "25")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"receipt store unavailable"}`)
	}))
	defer srv.Close()
	client := New(ports.ClientConfig{URL: srv.URL})
	err := client.AcceptDelivery(t.Context(), "delivery/17", ports.RunnerDeliveryAcceptance{
		ProtocolVersion: 1, PublicationGeneration: "pub-generation", ManifestDigest: "manifest-digest", ConsumerReceiptID: "receipt-id",
	})
	if method != http.MethodPost || path != "/api/v1/deliveries/delivery%2F17/accept" {
		t.Fatalf("accept request = %s %s", method, path)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatusCode() != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "receipt store unavailable") {
		t.Fatalf("accept error = %T %v, want surfaced 503", err, err)
	}
	var retry ports.RetryDeadline
	if !errors.As(err, &retry) || retry.RetryDeadline().Before(time.Now().Add(20*time.Second)) {
		t.Fatalf("accept outage lost Retry-After: %T %v", err, err)
	}
}
