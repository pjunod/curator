//go:build !linux && !darwin

package acquisition

import (
	"fmt"
	"os"
)

func deliveryFileGeneration(os.FileInfo) string          { return "" }
func deliveryFileStableIdentity(os.FileInfo) string      { return "" }
func deliveryDirectoryObjectIdentity(os.FileInfo) string { return "" }

// Platforms without a verified mount-source implementation fail closed for
// storage operations. Inode or device numbers alone are not durable proof.
func expectedStorageIdentity(path string) (string, error) {
	return "", fmt.Errorf("storage verification is unavailable on this platform for %s", path)
}
