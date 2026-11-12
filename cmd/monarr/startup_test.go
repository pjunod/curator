package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/config"
)

func TestServerWiresRecommendationLifecycleAndShutsDown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The server takes this bound listener: no window between picking the
	// port and binding it for another process on the runner to take it.
	port := listener.Addr().(*net.TCPAddr).Port
	t.Setenv("MONARR_EMBED_BINARY", "/missing-test-encoder")
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- serve(ctx, config.Config{Host: "127.0.0.1", Port: port, DataDir: dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)), listener)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("server did not stop")
		}
	})
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/metadata/recommendations/status"
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := client.Get(url)
		if err == nil {
			var payload map[string]any
			decodeErr := json.NewDecoder(response.Body).Decode(&payload)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK || decodeErr != nil {
				t.Fatalf("status: %d %v", response.StatusCode, decodeErr)
			}
			if payload["modelState"] != "disabled" {
				t.Fatalf("optional model was not disabled: %+v", payload)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
