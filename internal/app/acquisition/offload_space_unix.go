//go:build darwin || linux || freebsd

package acquisition

import (
	"fmt"
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

// offloadPathIdentity describes an existing storage directory or regular file.
// Directory timestamps are omitted because deleting a disjoint scope can change them.
func offloadPathIdentity(path string, file bool) string {
	info, err := os.Lstat(path)
	if err != nil || file && !info.Mode().IsRegular() {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	id := fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	if file {
		id += fmt.Sprintf(":%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	return id
}
