package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/buildinfo"
)

func TestNewClientSendsMonarrUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := NewClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	want := "Monarr/" + buildinfo.Version
	if got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
	if strings.Contains(got, "Go-http-client") {
		t.Errorf("outbound request still identifies as Go's default: %q", got)
	}
}

func TestExplicitUserAgentIsNotOverwritten(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "Custom/1.0")
	resp, err := NewClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got != "Custom/1.0" {
		t.Errorf("User-Agent = %q, want the caller's Custom/1.0", got)
	}
}
