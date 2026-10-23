package sqlite

import (
	"crypto/rand"
	"encoding/hex"
)

func newDownloadGenerationKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
