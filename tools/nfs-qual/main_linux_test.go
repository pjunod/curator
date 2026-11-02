//go:build linux

package main

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

func TestStorageErrorRemainsPrimaryWhenItAlsoCancelsCopy(t *testing.T) {
	if onlyCancellation(errors.Join(context.Canceled, syscall.EIO)) {
		t.Fatal("writeback EIO would be mislabeled as user cancellation")
	}
	if !onlyCancellation(errors.Join(context.Canceled, context.Canceled)) {
		t.Fatal("signal-only cancellation lost its distinct outcome")
	}
}
