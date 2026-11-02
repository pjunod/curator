package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func maintenanceServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MONARR_HOST", host)
	t.Setenv("MONARR_PORT", port)
	t.Setenv("MONARR_DATA_DIR", t.TempDir())
	return server
}

func TestMaintenanceClientBindsControlMethodOwnerAndCredentials(t *testing.T) {
	for _, op := range []string{"status", "drain", "abort"} {
		t.Run(op, func(t *testing.T) {
			maintenanceServer(t, func(w http.ResponseWriter, r *http.Request) {
				method := map[string]string{"status": "GET", "drain": "POST", "abort": "DELETE"}[op]
				if r.Method != method || r.URL.Path != "/api/v1/system/deployment-drain" || r.Header.Get("X-Api-Key") != "secret" {
					t.Errorf("control request %s %s", r.Method, r.URL)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if op != "status" && !bytes.Contains(raw, []byte(`"ownerToken":"owner"`)) {
					t.Errorf("owner missing: %s", raw)
				}
				if op == "abort" && !bytes.Contains(raw, []byte(`"generation":"generation"`)) {
					t.Errorf("generation missing: %s", raw)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"phase":"drained"}`)
			})
			var out bytes.Buffer
			if err := runMaintenance(context.Background(), []string{op}, strings.NewReader(`{"apiKey":"secret","ownerToken":"owner","generation":"generation"}`), &out); err != nil {
				t.Fatal(err)
			}
			if out.String() != "{\"phase\":\"drained\"}\n" {
				t.Fatal(out.String())
			}
		})
	}
}

func TestMaintenanceClientRejectsRedirectInvalidResponseAndBadArguments(t *testing.T) {
	for _, kind := range []string{"redirect", "unauthorized", "invalid JSON", "empty"} {
		t.Run(kind, func(t *testing.T) {
			maintenanceServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					http.Redirect(w, r, "http://127.0.0.1:1/wrong-node", http.StatusFound)
				case "unauthorized":
					w.WriteHeader(401)
				case "invalid JSON":
					_, _ = io.WriteString(w, "not json")
				case "empty":
				}
			})
			if err := runMaintenance(context.Background(), []string{"status"}, strings.NewReader("{}"), io.Discard); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
	for _, args := range [][]string{nil, {"status", "extra"}, {"unknown"}} {
		if err := runMaintenance(context.Background(), args, strings.NewReader("{}"), io.Discard); err == nil {
			t.Fatal("bad operation accepted")
		}
	}
	if err := runMaintenance(context.Background(), []string{"status"}, strings.NewReader("{"), io.Discard); err == nil {
		t.Fatal("bad credentials accepted")
	}
	t.Setenv("MONARR_PORT", "not-a-port")
	if err := runMaintenance(context.Background(), []string{"status"}, strings.NewReader("{}"), io.Discard); err == nil {
		t.Fatal("bad configuration accepted")
	}
	t.Setenv("MONARR_PORT", strconv.Itoa(1))
	t.Setenv("MONARR_HOST", "127.0.0.1")
	if err := runMaintenance(context.Background(), []string{"status"}, strings.NewReader("{}"), io.Discard); err == nil {
		t.Fatal("connection failure accepted")
	}
}

func TestMaintenanceClientResolvesWildcardHostsAndRejectsTruncatedBody(t *testing.T) {
	maintenanceServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"phase":"accepting"}`) })
	t.Setenv("MONARR_HOST", "0.0.0.0")
	if e := runMaintenance(t.Context(), []string{"status"}, strings.NewReader("{}"), io.Discard); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MONARR_HOST", "invalid\nhost")
	if e := runMaintenance(t.Context(), []string{"status"}, strings.NewReader("{}"), io.Discard); e == nil {
		t.Fatal("invalid request host accepted")
	}
	t.Setenv("MONARR_HOST", "::")
	t.Setenv("MONARR_PORT", "1")
	if e := runMaintenance(t.Context(), []string{"status"}, strings.NewReader("{}"), io.Discard); e == nil || !strings.Contains(e.Error(), "[::1]") {
		t.Fatalf("IPv6 wildcard was not bound to loopback: %v", e)
	}
	maintenanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, `{}`)
	})
	if e := runMaintenance(t.Context(), []string{"status"}, strings.NewReader("{}"), io.Discard); e == nil {
		t.Fatal("truncated maintenance response accepted")
	}
}
