package verify

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
