package ports

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewTransferToken returns an opaque, globally collision-resistant request
// token. The optional row id is for log readability only; callers may use 0
// when the database row has not been committed yet.
func NewTransferToken(rowID int64) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("create transfer token: %w", err)
	}
	return fmt.Sprintf("t-%d-%s", rowID, hex.EncodeToString(suffix[:])), nil
}
