package identity

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

func NewID() string {
	var id [16]byte
	_, _ = rand.Read(id[:]) // crypto/rand.Read cannot fail on supported Go versions.
	return hex.EncodeToString(id[:])
}
func ValidID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}
