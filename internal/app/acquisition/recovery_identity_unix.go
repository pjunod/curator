//go:build darwin || linux || freebsd

package acquisition

import (
	"os"
	"syscall"
)

func recoveryDirectoryIdentity(info os.FileInfo) any {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return []uint64{uint64(stat.Dev), uint64(stat.Ino)}
	}
	return []any{info.Name(), info.Mode().String()}
}

// fileDevice is the filesystem a file lives on.
func fileDevice(info os.FileInfo) (uint64, bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), true
	}
	return 0, false
}
