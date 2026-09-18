package crypto

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomHex reads nBytes from crypto/rand and returns a lowercase hex-encoded string.
func RandomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SessionToken returns a 64-character hex string (32 random bytes).
// Used as the session cookie value and Redis key suffix.
func SessionToken() (string, error) {
	return RandomHex(32)
}

// AvatarSuffix returns an 8-character hex string (4 random bytes).
// Used in avatar filenames to prevent enumeration.
func AvatarSuffix() (string, error) {
	return RandomHex(4)
}
