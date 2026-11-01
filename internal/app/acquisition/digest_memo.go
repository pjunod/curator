package acquisition

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Content digests are memoized by file generation.
//
// Field data 2026-11-01: proving one 2.9 GB obfuscated duplicate redundant
// took 250–280 s while a real 2.5 GB import took 31 s. Every attempt read the
// same bytes several times over network mounts — verifyDeliveryManifest
// hashed the payload, recheckDeliveryFiles hashed it again moments later, and
// the redundancy proof hashed the library file — and nothing was remembered
// between attempts, so a receipt retried a thousand times re-read its payload
// thousands of times. A 31 GB cleanup re-hashed both copies on every retry and
// held an import worker for over twenty minutes.
//
// A SHA-256 is a fact about bytes, and the bytes of a file cannot change
// without changing its generation: device, inode, size, and mtime and ctime
// to the nanosecond. ctime cannot be set by an ordinary write or utimes call.
// The one hole is timestamp granularity — a filesystem with coarse times can
// rewrite a file within one tick and leave every field equal — and it is the
// same hole git's index closes with its "racily clean" rule: a digest is only
// remembered when the file's last change is older than the moment hashing
// began by at least digestMemoRacyMargin. A file still being written is
// re-read every time until it settles.
//
// What is memoized is the digest of an identity, never a decision. Every
// caller still performs its own identity checks (same file, unchanged
// generation, no symlinks) before trusting a hit.
var (
	// digestMemoRacyMargin is held atomically so tests may shorten it while
	// other tests hash concurrently.
	digestMemoRacyMargin atomic.Int64
	digestMemoCapacity   = 8192

	// digestFullReads counts full-content hash reads, so tests can assert a
	// memo hit read nothing.
	digestFullReads atomic.Int64

	memo = &digestMemo{entries: map[string]string{}}
)

func init() { digestMemoRacyMargin.Store(int64(3 * time.Second)) }

type digestMemo struct {
	mu      sync.Mutex
	entries map[string]string
	order   []string
}

// digestMemoKey is the full generation plus the device, or "" where the
// platform cannot provide both.
func digestMemoKey(info os.FileInfo) string {
	device, ok := fileDevice(info)
	generation := deliveryFileGeneration(info)
	if !ok || generation == "" {
		return ""
	}
	return device + ":" + generation
}

// lookupDigest returns the remembered digest for exactly this generation.
func lookupDigest(info os.FileInfo) (string, bool) {
	key := digestMemoKey(info)
	if key == "" {
		return "", false
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	digest, ok := memo.entries[key]
	return digest, ok
}

// rememberDigest records a digest computed by reading the file in full,
// starting at hashStarted, when the file had already been stable for the
// racy margin. info must be the generation the read was verified against.
func rememberDigest(info os.FileInfo, digest string, hashStarted time.Time) {
	key := digestMemoKey(info)
	changed, ok := fileChangeTime(info)
	if key == "" || digest == "" || !ok || !changed.Before(hashStarted.Add(-time.Duration(digestMemoRacyMargin.Load()))) {
		return
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	if _, exists := memo.entries[key]; exists {
		memo.entries[key] = digest
		return
	}
	for len(memo.order) >= digestMemoCapacity {
		delete(memo.entries, memo.order[0])
		memo.order = memo.order[1:]
	}
	memo.entries[key] = digest
	memo.order = append(memo.order, key)
}
