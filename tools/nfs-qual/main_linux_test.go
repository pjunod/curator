//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStorageErrorRemainsPrimaryWhenItAlsoCancelsCopy(t *testing.T) {
	if onlyCancellation(errors.Join(context.Canceled, syscall.EIO)) {
		t.Fatal("writeback EIO would be mislabeled as user cancellation")
	}
	if !onlyCancellation(errors.Join(context.Canceled, context.Canceled)) {
		t.Fatal("signal-only cancellation lost its distinct outcome")
	}
}

func TestScratchPoliciesPreserveBytesAndExclusiveOwnership(t *testing.T) {
	data := bytes.Repeat([]byte("qualification fixture\n"), 160000)
	source := filepath.Join(t.TempDir(), "source")
	if e := os.WriteFile(source, data, 0400); e != nil {
		t.Fatal(e)
	}
	for _, policy := range []string{"whole", "periodic", "concurrent", "range"} {
		t.Run(policy, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "target")
			e := &experiment{start: time.Now()}
			e.primary.Store("starting")
			if err := e.run(t.Context(), source, target, int64(len(data)), policy, 512*1024, false); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("copy changed bytes: %v", err)
			}
			if e.bytes.Load() != int64(len(data)) {
				t.Fatal("progress did not cover exact payload")
			}
			if err := e.run(t.Context(), source, target, int64(len(data)), policy, 512*1024, false); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing target was not protected: %v", err)
			}
		})
	}
}
func TestScratchCancellationAndInvalidSourcesRetainTruthfulOutcomes(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("payload"), 0400); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"whole", "periodic", "concurrent", "range"} {
		t.Run(policy, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "partial")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			e := &experiment{start: time.Now()}
			e.primary.Store("starting")
			if err := e.run(ctx, source, target, 7, policy, 2, false); !onlyCancellation(err) {
				t.Fatalf("cancellation mislabeled: %v", err)
			}
			if info, err := os.Stat(target); err != nil || info.Size() != 0 {
				t.Fatalf("cancelled copy unexpectedly wrote: %v %v", info, err)
			}
		})
	}
	for _, input := range []string{source + "-missing", filepath.Dir(source), source} {
		e := &experiment{start: time.Now()}
		if err := e.run(t.Context(), input, filepath.Join(t.TempDir(), "never"), 8, "whole", 2, false); err == nil || onlyCancellation(err) {
			t.Fatalf("invalid source mislabeled: %v", err)
		}
	}
}
