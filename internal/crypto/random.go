// TODO: Cryptographically random string helpers.
//
// Used for:
//   - Session tokens (32 bytes → 64 hex chars)
//   - Avatar filename suffix (4 bytes → 8 hex chars)
//
// Public API:
//   func RandomHex(nBytes int) (string, error)
//     Reads nBytes from crypto/rand, returns lowercase hex-encoded string of length 2*nBytes.
//   func SessionToken() (string, error)       // 32 bytes → 64 hex
//   func AvatarSuffix() (string, error)       // 4 bytes → 8 hex
//
// Never use math/rand for these.
package crypto

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const (
	sessionTokenBytes = 32 // 256 bits of entropy -> 64 hex chars
	avatarSuffixBytes = 4  // only needs to defeat path guessing -> 8 hex chars
)

// RandomHex returns nBytes of cryptographically secure randomness, lowercase
// hex encoded, so the returned string is 2*nBytes long.
//
// This always reads from crypto/rand. math/rand is seeded predictably and its
// internal state is recoverable from a handful of outputs, which would make
// session tokens guessable.
func RandomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto: read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SessionToken returns a 64-character session token.
func SessionToken() (string, error) { return RandomHex(sessionTokenBytes) }

// AvatarSuffix returns an 8-character suffix for an avatar filename.
func AvatarSuffix() (string, error) { return RandomHex(avatarSuffixBytes) }
