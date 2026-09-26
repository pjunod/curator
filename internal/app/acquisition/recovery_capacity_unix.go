//go:build darwin || linux || freebsd

package acquisition

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func recoveryCapacity(path string, bytes int64) error {
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			return fmt.Errorf("library volume unavailable")
		}
		path = parent
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return err
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if available < uint64(bytes)+(1<<30) {
		return fmt.Errorf("library needs %d bytes plus 1 GiB reserve; %d available", bytes, available)
	}
	return nil
}
