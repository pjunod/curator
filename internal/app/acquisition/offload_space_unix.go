//go:build darwin || linux || freebsd

package acquisition

import (
	"os"
	"path/filepath"
	"syscall"
)

// offloadFreeBytes is the free space of the filesystem holding path (walking
// up to an existing parent), or -1 when it cannot be read.
func offloadFreeBytes(path string) int64 {
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			return -1
		}
		path = parent
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize) //nolint:unconvert // types differ per-OS
}

// offloadLinkCount is how many directory entries point at the file; above 1
// the bytes stay allocated until the other links (a seeding copy) go too.
func offloadLinkCount(path string) uint64 {
	info, err := os.Lstat(path)
	if err != nil {
		return 1
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // types differ per-OS
	}
	return 1
}

// completedCapacity observes the exact verified root; it never walks up to a
// different filesystem if a mount disappears.
func completedCapacity(path string) (*int64, *int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, nil
	}
	free, total := int64(st.Bavail)*int64(st.Bsize), int64(st.Blocks)*int64(st.Bsize) //nolint:unconvert // platform types differ
	if free < 0 || total < 0 {
		return nil, nil
	}
	return &free, &total
}
