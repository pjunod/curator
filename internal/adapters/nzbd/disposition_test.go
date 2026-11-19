package nzbd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

func TestDispositionWirePreservesCanonicalIdentityAndRefusesUnboundReplies(t *testing.T) {
	request := ports.RunnerDispositionRequest{RequestID: "curator-delete-attempt", Action: "delete", Transfer: "opaque-transfer", RunnerInstanceID: "original-owner", Generation: "admission-7", SourceArtifactID: "owned-source", SourceGeneration: "source-9"}
	for _, test := range []struct {
		name, state, mismatch string
		status                int
	}{
		{"pending", "pending", "", 202}, {"blocked", "blocked", "", 200}, {"verified", "verified", "", 200},
		{"wrong job", "verified", "jobId", 200}, {"wrong request", "verified", "requestId", 200},
		{"wrong transfer", "verified", "transfer", 200}, {"wrong owner", "verified", "runnerInstanceId", 200},
		{"wrong admission", "verified", "generation", 200}, {"unknown state", "finished", "", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer owner-token" || !strings.HasPrefix(r.Header.Get("X-Nzbd-Client"), "monarr/") {
					t.Errorf("owner credential/client identity missing")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/status" {
					_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"other", "job-disposition-v1"}})
					return
				}
				if r.Method != "POST" || r.URL.Path != "/api/v1/jobs/7/disposition" {
					t.Errorf("unexpected destructive endpoint: %s %s", r.Method, r.URL.Path)
				}
				calls++
				var received ports.RunnerDispositionRequest
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil || !reflect.DeepEqual(received, request) {
					t.Errorf("canonical body changed: %+v, %v", received, err)
				}
				response := map[string]any{"requestId": request.RequestID, "jobId": "7", "transfer": request.Transfer, "runnerInstanceId": request.RunnerInstanceID, "generation": request.Generation, "state": test.state}
				if test.mismatch != "" {
					response[test.mismatch] = "different"
				}
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			adapter := New(ports.ClientConfig{URL: server.URL + "/", Password: "owner-token"})
			for range 2 {
				got, err := adapter.DisposeJob(t.Context(), "7", request)
				if test.mismatch != "" || test.state == "finished" {
					if err == nil {
						t.Fatalf("unbound/unknown result accepted: %+v", got)
					}
				} else if err != nil || got.State != test.state {
					t.Fatalf("disposition = %+v, %v", got, err)
				}
			}
			if calls != 2 {
				t.Fatalf("retry changed native operation: calls=%d", calls)
			}
		})
	}
}

func TestDispositionCapabilityAndTransportFailureCannotBecomeSuccess(t *testing.T) {
	for _, mode := range []string{"unsupported", "status unavailable", "busy", "refused", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/status" {
					if mode == "status unavailable" {
						w.WriteHeader(503)
						return
					}
					if mode == "unsupported" {
						_, _ = w.Write([]byte(`{"capabilities":[]}`))
						return
					}
					_, _ = w.Write([]byte(`{"capabilities":["job-disposition-v1"]}`))
					return
				}
				posts++
				if mode == "busy" {
					w.WriteHeader(429)
				} else if mode == "refused" {
					w.WriteHeader(409)
				}
				_, _ = w.Write([]byte(`not a disposition`))
			}))
			defer server.Close()
			_, err := New(ports.ClientConfig{URL: server.URL}).DisposeJob(t.Context(), "7", ports.RunnerDispositionRequest{})
			if err == nil {
				t.Fatal("owner failure was accepted")
			}
			if (mode == "unsupported" || mode == "status unavailable") && posts != 0 {
				t.Fatal("destructive request sent without capability evidence")
			}
			if mode == "busy" || mode == "refused" {
				var api *APIError
				if !errors.As(err, &api) || (mode == "busy" && api.StatusCode != 429) || (mode == "refused" && api.StatusCode != 409) {
					t.Fatalf("HTTP outcome lost: %v", err)
				}
			}
		})
	}
}

func TestStorageVolumesCarryOwnerMeasurementAndObservationState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"version":"0.9.8","storage":[{"name":"source","label":"Downloads","path":"/owner/source","available_bytes":0,"total_bytes":500,"current":true},{"name":"offline","path":"/owner/missing","current":false}]}`))
	}))
	defer server.Close()
	got, err := New(ports.ClientConfig{URL: server.URL}).StorageVolumes(t.Context())
	if err != nil || len(got) != 2 || got[0].FreeBytes == nil || *got[0].FreeBytes != 0 || got[0].TotalBytes == nil || *got[0].TotalBytes != 500 || !got[0].Current || got[1].Current || got[1].FreeBytes != nil {
		t.Fatalf("owner observation changed: %+v, %v", got, err)
	}
}
