//go:build unix

package library

import "syscall"

// freeBytes reports the free space of the filesystem containing path,
// or 0 when it cannot be determined.
func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize) //nolint:unconvert // types differ per-OS
}
