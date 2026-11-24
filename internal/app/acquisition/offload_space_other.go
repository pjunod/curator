//go:build !darwin && !linux && !freebsd

package acquisition

import "os"

// offloadFreeBytes is unavailable on this platform.
func offloadFreeBytes(string) int64 { return -1 }

// offloadLinkCount is unavailable on this platform.
func offloadLinkCount(string) uint64 { return 1 }

func completedCapacity(string) (*int64, *int64) { return nil, nil }

// This platform cannot verify a file's storage identity.
func offloadPathIdentity(string, bool) string { return "" }

func offloadFileIdentity(os.FileInfo, bool) string { return "" }
func offloadInfoLinkCount(os.FileInfo) uint64      { return 1 }
