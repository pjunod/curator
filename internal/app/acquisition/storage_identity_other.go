//go:build !linux && !darwin

package acquisition

import (
	"fmt"
	"os"
	"time"
)

func deliveryFileGeneration(os.FileInfo) string          { return "" }
func deliveryFileStableIdentity(os.FileInfo) string      { return "" }
func deliveryDirectoryObjectIdentity(os.FileInfo) string { return "" }

// Platforms without a verified mount-source implementation fail closed for
// storage operations. Inode or device numbers alone are not durable proof.
func expectedStorageIdentity(path string) (string, error) {
	return "", fmt.Errorf("storage verification is unavailable on this platform for %s", path)
}

// The digest memo is off where file identity is unavailable.
func fileDevice(os.FileInfo) (string, bool)        { return "", false }
func fileChangeTime(os.FileInfo) (time.Time, bool) { return time.Time{}, false }
