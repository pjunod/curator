//go:build !darwin && !linux && !freebsd

package acquisition

import "os"

func recoveryDirectoryIdentity(info os.FileInfo) any {
	return []any{info.Name(), info.Mode().String(), info.ModTime().UnixNano()}
}

// fileDevice is unknown here: placements copy.
func fileDevice(os.FileInfo) (uint64, bool) { return 0, false }
