package nzbd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestNativeTorrentUsesIndependentJSONAdmissionAndRequiredCapabilities(t *testing.T) {
	for _, source := range []string{"magnet:?xt=urn:btih:fixture", "https://indexer.example/fixture.torrent"} {
		t.Run(source, func(t *testing.T) {
			var posted map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/status":
					_ = json.NewEncoder(w).Encode(map[string]any{"version": "fixture", "capabilities": []string{"deliveries-v1", NativeTorrentCapability, "request-token-idempotency-v1"}})
				case "/api/v1/jobs":
					if r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
						t.Errorf("unexpected native request %s %s %v", r.Method, r.URL, r.Header)
					}
					if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
						t.Error(err)
					}
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"id":42,"created":true}`))
				default:
					t.Errorf("unexpected route %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			client := NewTorrent(ports.ClientConfig{URL: server.URL})
			if err := client.Test(context.Background()); err != nil {
				t.Fatal(err)
			}
			supported, err := client.SupportsDeliveryV1(t.Context())
			if err != nil || !supported {
				t.Fatalf("cap=%v %v", supported, err)
			}
			handle, err := client.AddWithOptions(t.Context(), source, "movies", ports.AddOptions{Transfer: "fresh-token", DeliveryExpected: true, ManualAdditional: true, Priority: 100})
			if err != nil || handle != "42" {
				t.Fatalf("handle=%q err=%v", handle, err)
			}
			if posted["source"].(map[string]any)["uri"] != source || posted["priority"] != float64(100) || posted["category"] != "movies" {
				t.Fatalf("admission=%#v", posted)
			}
			params := posted["params"].(map[string]any)
			if params[TransferParam] != "fresh-token" || params[DeliveryParam] != "1" || params[ManualAdditionalParam] != "1" {
				t.Fatalf("params=%#v", params)
			}
		})
	}
}
func TestNativeTorrentMissingCapabilityRefusesBeforeAdmissionAndUsenetFallbackRemains(t *testing.T) {
	for _, capabilities := range [][]string{nil, {"deliveries-v1"}, {NativeTorrentCapability}} {
		posted := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/status" {
				_ = json.NewEncoder(w).Encode(map[string]any{"version": "fixture", "capabilities": capabilities})
				return
			}
			posted = true
			w.WriteHeader(500)
		}))
		client := NewTorrent(ports.ClientConfig{URL: server.URL})
		if err := client.Test(t.Context()); err == nil || !strings.Contains(err.Error(), NativeTorrentCapability) {
			t.Fatalf("missing capability error=%v", err)
		}
		if _, err := client.AddWithOptions(t.Context(), "magnet:?xt=urn:btih:fixture", "", ports.AddOptions{Transfer: "token", DeliveryExpected: true}); err == nil {
			t.Fatal("native add admitted without capability")
		}
		if posted {
			t.Fatal("unsupported native request reached any add/fallback route")
		}
		server.Close()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"version":"legacy"}`)) }))
	defer server.Close()
	supported, err := New(ports.ClientConfig{URL: server.URL}).SupportsDeliveryV1(t.Context())
	if err != nil || supported {
		t.Fatalf("existing Usenet legacy fallback changed: %v %v", supported, err)
	}
}
func TestNativeTorrentStatusesExcludeSeedDonorAndUsenetRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/jobs" {
			_, _ = w.Write([]byte(`{"jobs":[{"id":1,"name":"donor","torrent_role":"seed","status":"downloading"},{"id":2,"name":"acquisition","torrent_role":"acquisition","status":"downloading","params":[["monarr-transfer","token"]]},{"id":3,"name":"usenet","status":"downloading"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"entries":[{"job":4,"name":"retired child","torrent_role":"acquisition","status":"SUCCESS","params":[["monarr-transfer","old-token"]]},{"job":5,"name":"old donor","torrent_role":"seed","status":"SUCCESS"}]}`))
	}))
	defer server.Close()
	statuses, err := NewTorrent(ports.ClientConfig{URL: server.URL}).Statuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Handle != "2" || statuses[1].Handle != "4" {
		t.Fatalf("native statuses=%+v", statuses)
	}
	usenet, err := New(ports.ClientConfig{URL: server.URL}).Statuses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(usenet) != 1 || usenet[0].Handle != "3" {
		t.Fatalf("Usenet observed native donor/child: %+v", usenet)
	}
}

func TestNativeTorrentCapacitySeparatesNNTPAndPreservesStatusFailure(t *testing.T) {
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":"fixture status unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"version":"fixture","capabilities":["deliveries-v1","native-torrent-deliveries-v1"],"quota_reached":true,"blocked_servers":[1],"health_abort":true,"disk_low":true,"download_paused":true}`))
	}))
	defer server.Close()
	native := NewTorrent(ports.ClientConfig{URL: server.URL})
	capacity, err := native.Capacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if capacity.QuotaReached || capacity.BlockedServers != 0 || capacity.HealthAbort || !capacity.DiskLow || !capacity.Paused {
		t.Fatalf("native capacity=%+v", capacity)
	}
	legacy, err := New(ports.ClientConfig{URL: server.URL}).Capacity(t.Context())
	if err != nil || !legacy.QuotaReached || legacy.BlockedServers != 1 || !legacy.HealthAbort {
		t.Fatalf("Usenet capacity=%+v %v", legacy, err)
	}
	fail = true
	if _, err := native.Capacity(t.Context()); err == nil || !strings.Contains(err.Error(), "fixture status unavailable") {
		t.Fatalf("status error masked: %v", err)
	}
}

func TestNativeTorrentDurable202AdmissionAnd200ReplayPreserveCleanupPending(t *testing.T) {
	adds, histories := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			_, _ = w.Write([]byte(`{"version":"fixture","capabilities":["deliveries-v1","native-torrent-deliveries-v1"]}`))
		case "/api/v1/jobs":
			adds++
			if adds == 1 {
				w.WriteHeader(202)
			}
			_, _ = w.Write([]byte(`{"id":42,"created":false}`))
		case "/api/v1/jobs/42/actions/delete-files":
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":42,"pending":true}`))
		default:
			histories++
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	client := NewTorrent(ports.ClientConfig{URL: server.URL})
	options := ports.AddOptions{Transfer: "same-pending-request", DeliveryExpected: true, ManualAdditional: true}
	for attempt := 0; attempt < 2; attempt++ {
		handle, err := client.AddWithOptions(t.Context(), "magnet:?xt=urn:btih:fixture", "movies", options)
		if err != nil || handle != "42" {
			t.Fatalf("attempt %d handle=%q err=%v", attempt, handle, err)
		}
	}
	if err := client.Remove(t.Context(), "42", true); err == nil || !strings.Contains(err.Error(), "operation pending") {
		t.Fatalf("cleanup202 treated as done: %v", err)
	}
	if histories != 0 {
		t.Fatalf("pending cleanup fell back to other routes: %d", histories)
	}
}
