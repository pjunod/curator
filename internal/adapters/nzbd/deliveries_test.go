package nzbd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestSharedRunnerDeliveryFixtureDecodesActualDTOs(t *testing.T) {
	raw, err := os.ReadFile("testdata/curator_delivery_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Delivery         json.RawMessage `json:"delivery"`
		RequestAdmission json.RawMessage `json:"requestAdmission"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	var delivery runnerDeliveryWire
	if err := json.Unmarshal(fixture.Delivery, &delivery); err != nil {
		t.Fatal(err)
	}
	got, err := convertRunnerDelivery(delivery)
	if err != nil {
		t.Fatal(err)
	}
	if got.JobID != "123" || got.PublicationPath != "/downloads/completed/Épisode <one>/Release" || got.SourceGeneration != "source-generation-7" || got.TerminalOutcome != "FAILURE/HEALTH" || len(got.Files) != 1 || len(got.FileOutcomes) != 2 {
		t.Fatalf("delivery fixture conversion = %+v", got)
	}
	var cleanup struct {
		AttemptHistory []struct {
			Attempt int    `json:"attempt"`
			WorkMS  *int64 `json:"workMs"`
			WaitMS  int64  `json:"waitMs"`
			Outcome string `json:"outcome"`
		} `json:"attemptHistory"`
	}
	if err := json.Unmarshal(got.SourceCleanup, &cleanup); err != nil || len(cleanup.AttemptHistory) != 2 {
		t.Fatalf("Runner source cleanup attempt timing = %+v err=%v", cleanup, err)
	}
	if cleanup.AttemptHistory[0].Attempt != 1 || cleanup.AttemptHistory[0].WorkMS == nil || *cleanup.AttemptHistory[0].WorkMS != 2400 || cleanup.AttemptHistory[1].WaitMS != 97600 || cleanup.AttemptHistory[1].Outcome != "blocked" {
		t.Fatalf("Runner source cleanup timing conversion lost work/wait/outcome: %+v", cleanup.AttemptHistory)
	}
	var admission runnerRequestAdmissionWire
	if err := json.Unmarshal(fixture.RequestAdmission, &admission); err != nil {
		t.Fatal(err)
	}
	converted := admission.portValue()
	if converted.JobID != "123" || converted.SourceArtifactID != "source-β" || converted.SourceGeneration != "source-generation-7" || converted.TerminalOutcome != "FAILURE/HEALTH" || converted.SourceCleanupStatus != "blocked" {
		t.Fatalf("request admission fixture conversion = %+v", converted)
	}
}

func TestDeliveryProtocolRequestsAndValidation(t *testing.T) {
	var listed, accepted, lookedUp bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization header missing")
		}
		switch r.URL.Path {
		case "/api/v1/deliveries":
			listed = true
			if r.Method != http.MethodGet || r.URL.Query().Get("after") != "7" || r.URL.Query().Get("limit") != "100" {
				t.Errorf("unexpected list request: %s %s", r.Method, r.URL.String())
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"protocolVersion":1,"deliveries":[{"protocolVersion":1,"sequence":18,"deliveryId":"delivery-1","publicationGeneration":"gen-1","sourceArtifactId":"source-1","sourceGeneration":"source-gen-1","artifactId":"a1","jobId":17,"requestToken":"t-17","runnerInstanceId":"runner-1","publicationPath":"/completed/a","manifestDigest":"digest","files":[{"path":"movie.mkv","sizeBytes":1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}],"terminalOutcome":"PAR_FAILURE","failureStage":"par_verify","failureCause":"missing recovery blocks","keepSourceOnFailure":true,"fileOutcomes":[{"sourcePath":"The Sample Film.mkv","publicationPath":"The Sample Film.mkv","classification":"selected_output","outcome":"published","reason":"verified output","sourceCleanupStatus":"retained"},{"sourcePath":"é<&.par2","publicationPath":null,"classification":"repair_input","outcome":"unresolved","reason":"needed for later repair","sourceCleanupStatus":"retained"}],"readyAt":1700000000}],"nextCursor":18}`))
		case "/api/v1/deliveries/delivery-1/accept":
			accepted = true
			if r.Method != http.MethodPost {
				t.Errorf("accept method = %s", r.Method)
			}
			var got ports.RunnerDeliveryAcceptance
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.ProtocolVersion != 1 || got.PublicationGeneration != "gen-1" || got.ManifestDigest != "digest" || got.ConsumerReceiptID != "receipt" {
				t.Errorf("acceptance body = %+v", got)
			}
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/deliveries/delivery-1":
			lookedUp = true
			if r.Method != http.MethodGet {
				t.Errorf("lookup method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"protocolVersion":1,"sequence":18,"deliveryId":"delivery-1","publicationGeneration":"gen-1","sourceArtifactId":"source-1","sourceGeneration":"source-gen-1","artifactId":"a1","jobId":17,"requestToken":"t-17","runnerInstanceId":"runner-1","publicationPath":"/completed/a","manifestDigest":"digest","files":[{"path":"movie.mkv","sizeBytes":1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}],"terminalOutcome":"PAR_FAILURE","failureStage":"par_verify","failureCause":"missing recovery blocks","keepSourceOnFailure":true,"fileOutcomes":[{"sourcePath":"The Sample Film.mkv","publicationPath":"The Sample Film.mkv","classification":"selected_output","outcome":"published","reason":"verified output","sourceCleanupStatus":"removed"},{"sourcePath":"é<&.par2","publicationPath":null,"classification":"repair_input","outcome":"unresolved","reason":"needed for later repair","sourceCleanupStatus":"retained"}],"readyAt":1700000000,"sourceCleanup":{"state":"removed"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(ports.ClientConfig{Type: "nzbd", URL: srv.URL, Password: "secret"})
	page, err := c.ListDeliveries(t.Context(), 7, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 1 || page.Deliveries[0].JobID != "17" || !page.Deliveries[0].ReadyAt.Equal(time.Unix(1700000000, 0).UTC()) || page.Deliveries[0].TerminalOutcome != "PAR_FAILURE" || page.Deliveries[0].FailureStage != "par_verify" || !page.Deliveries[0].KeepSourceOnFailure || len(page.Deliveries[0].FileOutcomes) != 2 || page.Deliveries[0].SourceGeneration != "source-gen-1" {
		t.Fatalf("wire delivery conversion = %+v", page.Deliveries)
	}
	if err := c.AcceptDelivery(t.Context(), "delivery-1", ports.RunnerDeliveryAcceptance{ProtocolVersion: 1, PublicationGeneration: "gen-1", ManifestDigest: "digest", ConsumerReceiptID: "receipt"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.GetDelivery(t.Context(), "delivery-1")
	if err != nil || fresh.ProtocolVersion != 1 || fresh.DeliveryID != "delivery-1" || string(fresh.SourceCleanup) != `{"state":"removed"}` || len(fresh.FileOutcomes) != 2 || fresh.FileOutcomes[0].SourceCleanupStatus != "removed" || fresh.TerminalOutcome != "PAR_FAILURE" {
		t.Fatalf("source cleanup lookup=%+v err=%v", fresh, err)
	}
	if !listed || !accepted || !lookedUp {
		t.Fatalf("requests listed=%v accepted=%v lookedUp=%v", listed, accepted, lookedUp)
	}
	if _, err := c.ListDeliveries(t.Context(), 0, 101); err == nil {
		t.Fatal("out-of-range limit was accepted")
	}
}

func TestListDeliveriesRejectsUnknownProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"protocolVersion":2,"deliveries":[],"nextCursor":0}`))
	}))
	defer srv.Close()
	c := New(ports.ClientConfig{Type: "nzbd", URL: srv.URL})
	if _, err := c.ListDeliveries(t.Context(), 0, 10); err == nil {
		t.Fatal("unknown delivery protocol version was accepted")
	}
}

func TestDeliveryAcceptancePreservesConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"immutable delivery identity changed"}`))
	}))
	defer srv.Close()
	c := New(ports.ClientConfig{Type: "nzbd", URL: srv.URL})
	err := c.AcceptDelivery(t.Context(), "d1", ports.RunnerDeliveryAcceptance{ProtocolVersion: 1, PublicationGeneration: "g1", ManifestDigest: "digest", ConsumerReceiptID: "receipt"})
	if !errors.Is(err, ports.ErrDeliveryConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestRetryDeliverySourceCleanupUsesEmptyJSONPost(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/deliveries/d-1/source-cleanup/retry" {
			t.Errorf("unexpected source cleanup retry: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 0 {
			t.Errorf("retry body = %v, err=%v; want {}", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocolVersion":1,"sequence":1,"deliveryId":"d-1","publicationGeneration":"g1","artifactId":"a1","jobId":17,"requestToken":"t-17","runnerInstanceId":"r1","publicationPath":"/completed/r","manifestDigest":"0000000000000000000000000000000000000000000000000000000000000000","files":[{"path":"movie.mkv","sizeBytes":1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}],"readyAt":1}`))
	}))
	defer srv.Close()
	client := New(ports.ClientConfig{Type: "nzbd", URL: srv.URL})
	if _, err := client.RetryDeliverySourceCleanup(t.Context(), "d-1"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("Runner source cleanup retry request was not sent")
	}
}
