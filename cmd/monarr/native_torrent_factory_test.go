package main

import (
	"encoding/json"
	"github.com/pjunod/monarr/internal/ports"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProductionFactorySelectsNativeRunnerTorrentConstructor(t *testing.T) {
	posted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status" {
			_, _ = w.Write([]byte(`{"version":"fixture","capabilities":["deliveries-v1","native-torrent-deliveries-v1"]}`))
			return
		}
		if r.URL.Path != "/api/v1/jobs" || r.URL.RawQuery != "" {
			t.Errorf("wrong adapter route %s", r.URL)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		posted = body["source"] != nil
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":77}`))
	}))
	defer server.Close()
	client := downloadClientForConfig(ports.ClientConfig{Type: "runner-torrent", URL: server.URL})
	handle, err := client.(ports.ConfiguredAdder).AddWithOptions(t.Context(), "magnet:?xt=urn:btih:fixture", "movies", ports.AddOptions{Transfer: "factory-token", DeliveryExpected: true})
	if err != nil || handle != "77" || !posted {
		t.Fatalf("factory admission=%q %v posted=%v", handle, err, posted)
	}
	if ports.ProtocolOfClient("runner-torrent") != "torrent" || ports.DefaultPortFor("runner-torrent") != 6789 {
		t.Fatal("native type protocol or default port wrong")
	}
}
