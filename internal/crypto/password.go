// TODO: Password hashing wrapper (see §4.3).
//
// Uses golang.org/x/crypto/bcrypt at cost 10.
//
// Public API:
//   func HashPassword(plain string) ([]byte, error)
//     bcrypt.GenerateFromPassword([]byte(plain), 10)
//   func VerifyPassword(hash, plain []byte) error
//     bcrypt.CompareHashAndPassword(hash, plain)
//     Returns nil on match, error on mismatch.
//
// Note: bcrypt truncates input to 72 bytes — password validation should reject longer.
package crypto
