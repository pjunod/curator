package embedding

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVectorValidation(t *testing.T) {
	v := make([]float32, 384)
	v[0] = 1
	if !valid(v) {
		t.Fatal("unit vector rejected")
	}
	v[1] = float32(math.NaN())
	if valid(v) {
		t.Fatal("NaN admitted")
	}
	v[1] = 0
	v[0] = 2
	if valid(v) {
		t.Fatal("unnormalized vector admitted")
	}
	if valid(v[:383]) {
		t.Fatal("wrong dimension admitted")
	}
}

func blockedEncoder(t *testing.T) *Encoder {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "encoder")
	// A shell blocked reading stdin after the request simulates a wedged helper.
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile IFS= read -r line; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := New(t.TempDir(), binary)
	e.enabled = true
	e.state = "ready"
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestCancellationReapsHelperAndStartsCooldown(t *testing.T) {
	e := blockedEncoder(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.Encode(ctx, []string{"a story"}); err == nil {
		t.Fatal("blocked helper returned success")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.child != nil || e.state != "failed" || time.Until(e.cooldown) < 20*time.Second {
		t.Fatal("helper not reaped or cooldown missing")
	}
}

func TestDisableDuringEncodingKeepsDisabledState(t *testing.T) {
	e := blockedEncoder(t)
	done := make(chan error, 1)
	go func() { _, err := e.Encode(context.Background(), []string{"a story"}); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		e.mu.Lock()
		started := e.child != nil
		e.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		time.Sleep(time.Millisecond)
	}
	e.Enable(false)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disabled encoding succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("disable did not unblock encoding")
	}
	state, _ := e.State()
	if state != "disabled" {
		t.Fatalf("old encoding overwrote disabled state: %s", state)
	}
}
