package ports

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ExpectedFilesJSONSize counts the bytes in the array on the wire.
func ExpectedFilesJSONSize(files []ExpectedFile) int {
	if files == nil {
		files = []ExpectedFile{}
	}
	raw, _ := json.Marshal(files)
	return len(raw)
}

// ScanSubDeliveryKey is a sub-delivery's idempotency key (contract section 4.1):
// <transfer>/<generation>/<first 16 hex of sha256(directory)>. Cinema sees it
// with the attempt appended.
func ScanSubDeliveryKey(transfer, generation, dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return fmt.Sprintf("%s/%s/%s", transfer, generation, hex.EncodeToString(sum[:])[:16])
}
