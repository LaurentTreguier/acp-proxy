package acp

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomID returns a random 24-character hex string, used for the ids
// of API objects the proxy mints (completions, responses, messages).
func RandomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}
