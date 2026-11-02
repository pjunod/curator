//go:build linux

// nfs-qual is an isolated scratch-file experiment, never a publication path.
// All policies retain final fsync, join concurrent commit work, and close each
// descriptor once. Range writeback is measured, not treated as durability.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

type experiment struct {
	start   time.Time
	bytes   atomic.Int64
	primary atomic.Value
}

func (e *experiment) event(phase string, fields map[string]any) {
	switch phase {
	case "starting", "copy", "copy-returned", "final-sync", "final-sync-returned", "close", "close-returned", "finished":
		e.primary.Store(phase)
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["phase"] = phase
	fields["at"] = time.Now().UTC()
	fields["elapsed"] = time.Since(e.start).Seconds()
	fields["bytes"] = e.bytes.Load()
	fields["pid"] = os.Getpid()
	// One encoder/write per short line; phases from the committer can overlap.
	raw, err := json.Marshal(fields)
	if err == nil {
		_, _ = os.Stdout.Write(append(raw, '\n'))
	}
}

func main() {
	source := flag.String("source", "/source/payload.bin", "read-only task-owned source")
	target := flag.String("target", "", "new task-owned scratch file (exclusive create)")
	size := flag.Int64("size", 0, "payload bytes")
	policy := flag.String("policy", "whole", "whole|periodic|concurrent|range")
	window := flag.Int64("window", 1<<30, "writeback byte interval")
	closeStress := flag.Bool("close-stress", false, "skip final fsync ONLY for separately labeled close-writeback stress; never publish")
	flag.Parse()
	if *target == "" || *size <= 0 || *window <= 0 {
		fmt.Fprintln(os.Stderr, "target, positive size and window required")
		os.Exit(1)
	}
	if *policy != "whole" && *policy != "periodic" && *policy != "concurrent" && *policy != "range" {
		fmt.Fprintln(os.Stderr, "unknown policy")
		os.Exit(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &experiment{start: time.Now()}
	e.primary.Store("starting")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	stopSignals, signalsDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(signalsDone)
		select {
		case received := <-signals:
			e.event("signal-received", map[string]any{"signal": received.String(), "signal_number": int(received.(syscall.Signal)), "observed_phase": e.primary.Load()})
			cancel()
		case <-stopSignals:
		}
	}()
	e.event("starting", map[string]any{"policy": *policy, "window": *window, "size": *size, "close_stress": *closeStress, "external_cancelled": false})
	err := e.run(ctx, *source, *target, *size, *policy, *window, *closeStress)
	signal.Stop(signals)
	close(stopSignals)
	<-signalsDone
	result := "completed"
	code := 0
	if err != nil {
		result = "failed"
		code = 1
		if onlyCancellation(err) {
			result = "cancelled"
			code = 2
		}
	}
	fields := map[string]any{"result": result, "policy": *policy, "window": *window, "close_stress": *closeStress, "external_cancelled": ctx.Err() != nil}
	if err != nil {
		fields["error"] = err.Error()
	}
	e.event("finished", fields)
	os.Exit(code)
}

func (e *experiment) run(ctx context.Context, source, target string, size int64, policy string, window int64, closeStress bool) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	info, err := src.Stat()
	if err != nil {
		_ = src.Close()
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < size {
		_ = src.Close()
		return fmt.Errorf("source must be a regular file of at least %d bytes", size)
	}
	sourceIdentity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.Join(fmt.Errorf("source identity unavailable"), src.Close())
	}
	e.event("source-opened", map[string]any{"device": sourceIdentity.Dev, "inode": sourceIdentity.Ino})
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(err, src.Close())
	}
	created, statErr := dst.Stat()
	if statErr != nil {
		return errors.Join(statErr, dst.Close(), src.Close())
	}
	identity, ok := created.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.Join(fmt.Errorf("target identity unavailable"), dst.Close(), src.Close())
	}
	e.event("target-created", map[string]any{"device": identity.Dev, "inode": identity.Ino})
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	commits := make(chan struct{}, 1)
	committerDone := make(chan error, 1)
	if policy == "concurrent" {
		go func() {
			var commitErr error
			for range commits {
				if commitErr != nil || workCtx.Err() != nil {
					continue
				}
				e.event("concurrent-sync-start", nil)
				began := time.Now()
				commitErr = syscall.Fdatasync(int(dst.Fd()))
				e.event("concurrent-sync-end", map[string]any{"duration": time.Since(began).Seconds()})
				if commitErr != nil {
					cancel()
				}
			}
			committerDone <- commitErr
		}()
	}
	stopProgress := make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopProgress:
				return
			case <-ticker.C:
				e.event("progress", nil)
			}
		}
	}()
	defer func() { close(stopProgress); <-progressDone }()
	e.event("copy", map[string]any{"policy": policy, "window": window, "size": size})
	buffer := make([]byte, 1<<20)
	digest := sha256.New()
	var written, lastWindow int64
	previousRangeStart, previousRangeBytes := int64(0), int64(0)
	var copyErr error
	for written < size {
		if copyErr = workCtx.Err(); copyErr != nil {
			break
		}
		next := min(int64(len(buffer)), size-written)
		n, readErr := io.ReadFull(src, buffer[:next])
		if readErr != nil {
			copyErr = readErr
			break
		}
		count, writeErr := dst.Write(buffer[:n])
		_, _ = digest.Write(buffer[:count])
		written += int64(count)
		e.bytes.Store(written)
		if writeErr != nil {
			copyErr = writeErr
			break
		}
		if count != n {
			copyErr = io.ErrShortWrite
			break
		}
		if written-lastWindow >= window || written == size {
			if copyErr = workCtx.Err(); copyErr != nil {
				break
			}
			switch policy {
			case "periodic":
				e.event("periodic-sync-start", nil)
				began := time.Now()
				copyErr = syscall.Fdatasync(int(dst.Fd()))
				e.event("periodic-sync-end", map[string]any{"duration": time.Since(began).Seconds()})
			case "concurrent":
				select {
				case commits <- struct{}{}:
				default:
					e.event("concurrent-sync-coalesced", nil)
				}
			case "range":
				copyErr = syscall.SyncFileRange(int(dst.Fd()), lastWindow, written-lastWindow, 2) // WRITE
				if copyErr == nil && previousRangeBytes > 0 {
					copyErr = syscall.SyncFileRange(int(dst.Fd()), previousRangeStart, previousRangeBytes, 1|4)
				} // WAIT_BEFORE|WAIT_AFTER
				previousRangeStart, previousRangeBytes = lastWindow, written-lastWindow
			}
			lastWindow = written
			if copyErr != nil {
				break
			}
		}
	}
	e.event("copy-returned", map[string]any{"duration": time.Since(e.start).Seconds(), "sha256": fmt.Sprintf("%x", digest.Sum(nil))})
	if policy == "concurrent" {
		close(commits)
		copyErr = errors.Join(copyErr, <-committerDone)
	}
	var syncErr error
	if copyErr == nil {
		copyErr = workCtx.Err()
	}
	if copyErr == nil && !closeStress {
		e.event("final-sync", nil)
		began := time.Now()
		syncErr = dst.Sync()
		e.event("final-sync-returned", map[string]any{"duration": time.Since(began).Seconds(), "performed": true})
	} else {
		reason := "copy_or_cancel_error"
		if closeStress && copyErr == nil {
			reason = "close_stress"
		}
		e.event("final-sync-skipped", map[string]any{"reason": reason, "performed": false})
	}
	e.event("close", nil)
	began := time.Now()
	closeErr := dst.Close()
	e.event("close-returned", map[string]any{"duration": time.Since(began).Seconds()})
	sourceCloseErr := src.Close()
	if ctx.Err() != nil {
		copyErr = errors.Join(copyErr, ctx.Err())
	}
	return errors.Join(copyErr, syncErr, closeErr, sourceCloseErr)
}

func onlyCancellation(err error) bool {
	if err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyCancellation(wrapped.Unwrap())
	}
	return false
}
