//go:build !darwin && !linux && !freebsd

package acquisition

// offloadFreeBytes is unavailable on this platform.
func offloadFreeBytes(string) int64 { return -1 }

// offloadLinkCount is unavailable on this platform.
func offloadLinkCount(string) uint64 { return 1 }

// This platform cannot verify a file's storage identity.
func offloadPathIdentity(string, bool) string { return "" }
