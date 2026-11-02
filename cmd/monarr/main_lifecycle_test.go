package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercise dispatch through the real entrypoint: helpers must not start a
// second service, and the service must retain status access while draining.
func TestMainDispatchKeepsMaintenanceSeparateAndJoinsAfterTerm(t *testing.T) {
	originalArgs, originalInput, originalLog := os.Args, os.Stdin, slog.Default()
	t.Cleanup(func() { os.Args = originalArgs; os.Stdin = originalInput; slog.SetDefault(originalLog) })
	t.Setenv("MONARR_CONFIG", t.TempDir()+"/missing-config")
	os.Args = []string{"monarr", "advertise", "-h"}
	main() // Help needs no DB/config.
	t.Setenv("MONARR_CONFIG", "")
	maintenanceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system/deployment-drain" {
			t.Error(r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"phase":"accepting"}`)
	})
	// A maintenance invocation against an occupied API endpoint must return without trying
	// to bind its port or touch startup. Credentials stay on stdin.
	input, e := os.CreateTemp(t.TempDir(), "maintenance-input")
	if e != nil {
		t.Fatal(e)
	}
	defer input.Close()
	if _, e = input.WriteString("{}"); e != nil {
		t.Fatal(e)
	}
	if _, e = input.Seek(0, io.SeekStart); e != nil {
		t.Fatal(e)
	}
	os.Stdin = input
	os.Args = []string{"monarr", "maintenance", "status"}
	main()
	os.Args = []string{"monarr"}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if e = listener.Close(); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MONARR_HOST", "127.0.0.1")
	t.Setenv("MONARR_PORT", strconv.Itoa(port))
	t.Setenv("MONARR_DATA_DIR", shutdownTestDataDir(t))
	t.Setenv("MONARR_EMBED_BINARY", "/missing-test-encoder")
	t.Setenv("MONARR_LOG_LEVEL", "error")
	os.Args = []string{"monarr"}
	// Keep TERM caught until the owned service has joined, including an
	// unexpected early return between the done check and Signal below.
	termGuard := make(chan os.Signal, 1)
	signal.Notify(termGuard, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(termGuard) })
	done := make(chan struct{})
	go func() { defer close(done); main() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		select {
		case <-done:
			stopped = true
			return
		default:
		}
		process, err := os.FindProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		if err = process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			stopped = true
		case <-time.After(10 * time.Second):
			// Returning here would run environment/global cleanups under a
			// live service. Fail this owned test process before restoration.
			t.Log("entrypoint did not join workers after TERM; refusing global restoration under live work")
			os.Exit(1)
		}
	}
	t.Cleanup(stop)
	client := &http.Client{Timeout: time.Second}
	base := "http://127.0.0.1:" + strconv.Itoa(port) + "/api/v1"
	deadline := time.Now().Add(60 * time.Second)
	for {
		r, err := client.Get(base + "/system/status")
		if err == nil {
			_ = r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatal(r.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	payload := []byte(`{"ownerToken":"` + strings.Repeat("c1", 32) + `"}`)
	response, e := client.Post(base+"/system/deployment-drain", "application/json", bytes.NewReader(payload))
	if e != nil {
		t.Fatal(e)
	}
	var status struct{ Phase string }
	e = json.NewDecoder(response.Body).Decode(&status)
	_ = response.Body.Close()
	if e != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("drain %s %v", response.Status, e)
	}
	response, e = client.Post(base+"/rootfolders", "application/json", strings.NewReader(`{"kind":"movie","path":"/ignored"}`))
	if e != nil {
		t.Fatal(e)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("drain admitted mutation", response.Status)
	}
	response, e = client.Get(base + "/queue")
	if e != nil {
		t.Fatal(e)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("drain lost status", response.Status)
	}
	stop()
}
