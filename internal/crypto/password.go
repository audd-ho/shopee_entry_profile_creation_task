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

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// ErrPasswordMismatch is returned when a password does not match its hash. It
// replaces bcrypt's own error so callers never branch on a bcrypt-specific
// type, and so a mismatch reads differently from a genuine failure.
var ErrPasswordMismatch = errors.New("crypto: password does not match")

// MaxPasswordBytes is bcrypt's hard input limit. Longer inputs are rejected
// outright by modern x/crypto rather than silently truncated, so validation
// must enforce this before a password ever reaches here.
const MaxPasswordBytes = 72

// HashPassword hashes a password at bcrypt's default cost (10).
func HashPassword(plain string) ([]byte, error) {
	return HashPasswordWithCost(plain, bcrypt.DefaultCost)
}

// HashPasswordWithCost hashes at an explicit cost, so the value in
// config.Bcrypt.Cost is actually honoured rather than duplicated as a literal.
//
// The cost is the log2 of the number of rounds: every increment doubles the
// work. That slowness is the point — it is what makes an offline attack against
// a stolen hash expensive — so it must never be "optimised" for the login path.
func HashPasswordWithCost(plain string, cost int) ([]byte, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return nil, fmt.Errorf("crypto: hash password: %w", err)
	}
	return hash, nil
}

// VerifyPassword reports whether plain matches hash, returning nil on a match
// and ErrPasswordMismatch otherwise.
//
// bcrypt's own comparison is constant-time with respect to the hash contents,
// so this does not leak how much of the password was correct.
func VerifyPassword(hash, plain []byte) error {
	err := bcrypt.CompareHashAndPassword(hash, plain)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return ErrPasswordMismatch
	default:
		// A malformed or truncated hash in the database, not a wrong password.
		return fmt.Errorf("crypto: verify password: %w", err)
	}
}

// dummyHashes caches one throwaway hash per cost, so DummyVerify performs
// exactly one bcrypt comparison — the same work as a real verify — rather than
// a generate plus a compare, which would take twice as long and be just as
// distinguishable as taking no time at all.
var dummyHashes sync.Map // int cost -> []byte hash

// DummyVerify performs a bcrypt comparison that always fails, costing the same
// as a real VerifyPassword at that cost.
//
// Login calls this when the username does not exist. Without it a missing user
// answers in microseconds while a wrong password takes ~75ms, and that gap is a
// reliable oracle for which accounts exist. The uniform error message alone
// does not close the hole; the timing has to match too.
func DummyVerify(cost int) {
	hash, ok := dummyHashes.Load(cost)
	if !ok {
		generated, err := bcrypt.GenerateFromPassword([]byte("dummy password"), cost)
		if err != nil {
			return
		}
		hash, _ = dummyHashes.LoadOrStore(cost, generated)
	}
	_ = bcrypt.CompareHashAndPassword(hash.([]byte), []byte("wrong password"))
}
