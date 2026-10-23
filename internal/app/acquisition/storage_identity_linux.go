//go:build linux

package acquisition

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func deliveryFileGeneration(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", st.Ino, st.Size, st.Mtim.Sec, st.Mtim.Nsec, st.Ctim.Sec*1_000_000_000+st.Ctim.Nsec)
}

func deliveryFileStableIdentity(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return ""
	}
	return fmt.Sprintf("linux-stable-object-v1:%d", st.Ino)
}

// Directory identity is paired with the persisted storage-source identity.
// Device numbers are omitted so ordinary remounts remain valid while a new
// directory created at the same payload path cannot inherit cleanup rights.
func deliveryDirectoryObjectIdentity(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return ""
	}
	return fmt.Sprintf("linux-directory-v1:%d", st.Ino)
}

// expectedStorageIdentity records the mounted storage behind a configured
// root. Device numbers and mount IDs are intentionally omitted because both
// can change during an ordinary remount.
func expectedStorageIdentity(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("expected storage is unavailable at %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("expected storage path is not a directory: %s", path)
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", fmt.Errorf("storage mount information is unavailable: %w", err)
	}

	var stat unix.Statx_t
	statErr := unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_MNT_ID, &stat)
	var mount storageMount
	if statErr == nil && stat.Mask&unix.STATX_MNT_ID != 0 {
		mount, err = storageMountForPathMountID(f, filepath.Clean(path), stat.Mnt_id)
	} else if statErr == nil || errors.Is(statErr, unix.ENOSYS) || errors.Is(statErr, unix.EINVAL) {
		// Older kernels may not expose STATX_MNT_ID. The parser accepts only a
		// unique deepest covering mount in this fallback mode.
		mount, err = storageMountForPath(f, filepath.Clean(path))
	} else {
		err = fmt.Errorf("identify visible storage mount at %s: %w", path, statErr)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return "", errors.Join(err, closeErr)
	}
	return mount.identity(), nil
}

type storageMount struct {
	mountID                          uint64
	root, mountpoint, fsType, source string
}

func (m storageMount) identity() string {
	// The mountpoint is used to find the filesystem covering this path, but is
	// not part of its identity: bind aliases can expose the same subtree at a
	// different target. Mount source plus filesystem root/type distinguish the
	// storage while remaining stable across mount IDs and device renumbering.
	return strings.Join([]string{"linux-v1", m.fsType, m.source, m.root}, "\x00")
}

// storageMountForPath chooses the most specific mountpoint containing path.
// mountinfo uses octal escapes for whitespace, backslashes, and tabs.
func storageMountForPath(input io.Reader, path string) (storageMount, error) {
	return storageMountForPathMountID(input, path, 0)
}

func storageMountForPathMountID(input io.Reader, path string, visibleMountID uint64) (storageMount, error) {
	path = filepath.Clean(path)
	scanner := bufio.NewScanner(input)
	var best storageMount
	bestLen := -1
	equalBest := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+3 >= len(fields) {
			continue
		}
		mountID, parseErr := parseMountID(fields[0])
		if parseErr != nil || visibleMountID != 0 && mountID != visibleMountID {
			continue
		}
		root := unescapeMountInfo(fields[3])
		mountpoint := unescapeMountInfo(fields[4])
		if !pathWithinMount(mountpoint, path) {
			continue
		}
		if len(mountpoint) < bestLen {
			continue
		}
		if len(mountpoint) == bestLen && bestLen >= 0 {
			equalBest = true
			continue
		}
		best = storageMount{
			mountID:    mountID,
			root:       root,
			mountpoint: mountpoint,
			fsType:     fields[separator+1],
			source:     unescapeMountInfo(fields[separator+2]),
		}
		bestLen = len(mountpoint)
		equalBest = false
	}
	if err := scanner.Err(); err != nil {
		return storageMount{}, fmt.Errorf("read storage mount information: %w", err)
	}
	if bestLen < 0 {
		return storageMount{}, fmt.Errorf("storage mount information does not cover %s", path)
	}
	if visibleMountID != 0 && best.mountID != visibleMountID {
		return storageMount{}, fmt.Errorf("visible storage mount %d was not found for %s", visibleMountID, path)
	}
	if equalBest {
		return storageMount{}, fmt.Errorf("storage mount visibility is ambiguous for %s", path)
	}
	return best, nil
}

func parseMountID(value string) (uint64, error) {
	var id uint64
	if _, err := fmt.Sscanf(value, "%d", &id); err != nil {
		return 0, fmt.Errorf("invalid mount ID %q: %w", value, err)
	}
	return id, nil
}

func pathWithinMount(mountpoint, path string) bool {
	if mountpoint == "/" {
		return filepath.IsAbs(path)
	}
	return path == mountpoint || strings.HasPrefix(path, mountpoint+string(filepath.Separator))
}

func unescapeMountInfo(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			if value[i+1] >= '0' && value[i+1] <= '7' && value[i+2] >= '0' && value[i+2] <= '7' && value[i+3] >= '0' && value[i+3] <= '7' {
				b.WriteByte((value[i+1]-'0')*64 + (value[i+2]-'0')*8 + (value[i+3] - '0'))
				i += 3
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}
