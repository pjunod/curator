//go:build darwin

package acquisition

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func deliveryFileGeneration(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", st.Ino, st.Size, st.Mtimespec.Sec, st.Mtimespec.Nsec, st.Ctimespec.Sec*1_000_000_000+st.Ctimespec.Nsec)
}

func deliveryFileStableIdentity(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return ""
	}
	return fmt.Sprintf("darwin-stable-object-v1:%d", st.Ino)
}

func deliveryDirectoryObjectIdentity(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return ""
	}
	return fmt.Sprintf("darwin-directory-v1:%d", st.Ino)
}

// macOS exposes the mounted-from source, target, and filesystem type through
// statfs. Fsid and inode values are intentionally excluded because they can
// be reassigned by remounts.
func expectedStorageIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("expected storage is unavailable at %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("expected storage path is not a directory: %s", path)
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return "", fmt.Errorf("inspect expected storage at %s: %w", path, err)
	}
	return strings.Join([]string{
		"darwin-v1",
		statfsString(stats.Fstypename[:]),
		statfsString(stats.Mntfromname[:]),
		statfsString(stats.Mntonname[:]),
	}, "\x00"), nil
}

func statfsString(value []int8) string {
	bytes := make([]byte, 0, len(value))
	for _, b := range value {
		if b == 0 {
			break
		}
		bytes = append(bytes, byte(b))
	}
	return string(bytes)
}
