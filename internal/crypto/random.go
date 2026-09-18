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
