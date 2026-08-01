package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestRunAdvertisePublishesOnlyTheRequestedHostEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var gotName, gotVersion string
	var gotPort int
	stopped := false
	err := runAdvertise(ctx, []string{"--name", "Rack Monarr", "--port", "8787"}, io.Discard,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(name string, port int, version string, _ *slog.Logger) (func(), error) {
			gotName, gotPort, gotVersion = name, port, version
			return func() { stopped = true }, nil
		})
	if err != nil {
		t.Fatalf("runAdvertise: %v", err)
	}
	if gotName != "Rack Monarr" || gotPort != 8787 || gotVersion == "" {
		t.Fatalf("published name=%q port=%d version=%q", gotName, gotPort, gotVersion)
	}
	if !stopped {
		t.Fatal("advertiser did not stop when its context ended")
	}
}

func TestParseAdvertiseOptionsRejectsInvalidPortsAndArguments(t *testing.T) {
	for _, args := range [][]string{{"--port", "0"}, {"--port", "70000"}, {"extra"}} {
		if _, err := parseAdvertiseOptions(args, io.Discard); err == nil {
			t.Fatalf("parseAdvertiseOptions(%v) succeeded", args)
		}
	}
}
