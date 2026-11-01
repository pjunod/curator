package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/config"
)

func TestListenerFailureCancelsAndJoinsWorkersWithoutSignal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	dataDir := t.TempDir()
	go func() {
		finished <- run(ctx, config.Config{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, DataDir: dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case err := <-finished:
		var op *net.OpError
		if !errors.As(err, &op) || op.Op != "listen" {
			t.Fatalf("run = %v, want listen failure", err)
		}
		if ctx.Err() != nil {
			t.Fatal("test supplied cancellation instead of listener failure")
		}
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("workers leaked after cleanup cancellation")
		}
		t.Fatal("listener failure waited for an external signal")
	}
}

func TestRunShutsDownWithConnectedEventStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	dataDir := t.TempDir()
	go func() {
		finished <- run(ctx, config.Config{Host: "127.0.0.1", Port: port, DataDir: dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	var body io.ReadCloser
	joined := false
	t.Cleanup(func() {
		cancel()
		if body != nil {
			_ = body.Close()
		}
		if !joined {
			select {
			case <-finished:
			case <-time.After(15 * time.Second):
				t.Error("run did not stop")
			}
		}
	})
	client := &http.Client{Timeout: 5 * time.Second}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1/events"
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			body = resp.Body
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("stream = %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := bufio.NewReader(body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-finished:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connected SSE consumed the HTTP shutdown grace")
	}
}
