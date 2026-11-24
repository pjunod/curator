package nzbd

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// The release-probe contract (Runner docs/INTEGRATION.md, ADR 0031 §6):
// capability negotiation through /api/v1/status, the URL and byte count on
// POST /api/v1/probe, a base64 head back, or a refusal with its reason.
func TestReleaseProbeContract(t *testing.T) {
	head := []byte{0x1a, 0x45, 0xdf, 0xa3, 1, 2, 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			_, _ = w.Write([]byte(`{"version":"0.2.0","capabilities":["deliveries-v1","release-probe-v1"]}`))
		case "/api/v1/probe":
			if r.Method != http.MethodPost || r.URL.Query().Get("bytes") != "4194304" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			switch r.URL.Query().Get("url") {
			case "https://indexer/a":
				_, _ = w.Write([]byte(`{"status":"ok","file":"Title.mkv","file_size":76665520128,"path":["yenc","rar-store"],"articles":6,"articles_missing":0,"bytes":7,"head":"` +
					base64.StdEncoding.EncodeToString(head) + `"}`))
			default:
				_, _ = w.Write([]byte(`{"status":"unavailable","reason":"encrypted","detail":"Title.part1.rar is password-protected"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(ports.ClientConfig{Type: "nzbd", URL: srv.URL, Password: "token"})
	ctx := context.Background()

	ok, err := c.SupportsReleaseProbe(ctx)
	if err != nil || !ok {
		t.Fatalf("SupportsReleaseProbe = %v, %v", ok, err)
	}
	h, err := c.ProbeRelease(ctx, "https://indexer/a", 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if h.File != "Title.mkv" || h.FileSize != 76665520128 || len(h.Path) != 2 || h.Articles != 6 || string(h.Bytes) != string(head) {
		t.Errorf("head = %+v", h)
	}
	_, err = c.ProbeRelease(ctx, "https://indexer/b", 4<<20)
	var refusal *ports.ProbeRefusal
	if !errors.As(err, &refusal) || refusal.Reason != ports.ProbeEncrypted {
		t.Fatalf("refusal = %v, want encrypted", err)
	}
}

// An older Runner without the capability is simply never asked.
func TestReleaseProbeNotAdvertised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.1.9","capabilities":["deliveries-v1"]}`))
	}))
	defer srv.Close()
	ok, err := New(ports.ClientConfig{Type: "nzbd", URL: srv.URL}).SupportsReleaseProbe(context.Background())
	if err != nil || ok {
		t.Fatalf("SupportsReleaseProbe = %v, %v; want false without the capability", ok, err)
	}
}

// The largest head a profile may ask for (16 MiB) is about 21.4 MiB as
// base64 JSON: past the adapter's default 8 MiB response bound, which once
// failed every probe above about 6 MiB.
func TestReleaseProbeAcceptsTheLargestHead(t *testing.T) {
	head := make([]byte, 16<<20)
	copy(head, []byte{0x1a, 0x45, 0xdf, 0xa3})
	body := `{"status":"ok","file":"Title.mkv","file_size":1,"path":["yenc"],"articles":22,"head":"` +
		base64.StdEncoding.EncodeToString(head) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(ports.ClientConfig{Type: "nzbd", URL: srv.URL, Password: "token"})
	h, err := c.ProbeRelease(context.Background(), "https://indexer/a", 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Bytes) != len(head) {
		t.Fatalf("head = %d bytes, want %d", len(h.Bytes), len(head))
	}
}
