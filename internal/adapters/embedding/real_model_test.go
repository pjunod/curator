package embedding

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit local qualification; the normal suite requires no downloaded model.
func TestVerifiedRealHelper(t *testing.T) {
	model, binary := os.Getenv("MONARR_TEST_MODEL_DIR"), os.Getenv("MONARR_TEST_EMBED_BINARY")
	if model == "" || binary == "" {
		t.Skip("set explicit verified model and helper paths")
	}
	e := New(t.TempDir(), binary)
	defer func() { _ = e.Close() }()
	if err := os.MkdirAll(e.dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(model, f.name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(e.dir, f.name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	e.Enable(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	texts := []string{"Gay men fall in love and build an adult relationship.", "A political drama about a government conspiracy."}
	vectors, err := e.Encode(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 2 || !valid(vectors[0]) || !valid(vectors[1]) {
		t.Fatal("invalid real model response")
	}
	e.Enable(false)
	e.mu.Lock()
	child := e.child
	e.mu.Unlock()
	if child != nil {
		t.Fatal("disabling left helper running")
	}
	state, _ := e.State()
	if state != "disabled" {
		t.Fatal(state)
	}
}
