//go:build !darwin && !linux && !freebsd

package acquisition

// offloadFreeBytes is unavailable on this platform.
func offloadFreeBytes(string) int64 { return -1 }

// offloadLinkCount is unavailable on this platform.
func offloadLinkCount(string) uint64 { return 1 }
