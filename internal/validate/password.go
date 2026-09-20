// TODO: Password validation.
//
// Rules (see §4.10):
//   - Min length: 8
//   - Max length: 72 (bcrypt's hard limit)
//   - Reject all-same-char (trivial)
//   - Do NOT enforce complexity rules beyond length (NIST guidance)
//
// Public API:
//   func Password(s string) error
package validate

import (
	"errors"
	"fmt"
)

const (
	PasswordMinBytes = 8

	// PasswordMaxBytes is bcrypt's hard input limit. Modern x/crypto returns an
	// error rather than silently truncating, so a longer password would fail at
	// hashing time with an opaque message instead of a clear validation error.
	PasswordMaxBytes = 72
)

var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d bytes", PasswordMinBytes)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", PasswordMaxBytes)
	ErrPasswordTrivial  = errors.New("password must not be a single repeated character")
)

// Password reports whether s is an acceptable password.
//
// Length is measured in BYTES, not runes, because bcrypt's 72 is a byte limit:
// an 18-character emoji password is already at the ceiling.
//
// There are deliberately no composition rules — no "must contain a digit and a
// symbol". Current NIST guidance drops them because they push users toward
// predictable mutations (Password1!) while blocking genuinely strong
// passphrases. Length plus a rejection of outright degenerate input is the
// whole policy.
func Password(s string) error {
	if len(s) < PasswordMinBytes {
		return ErrPasswordTooShort
	}
	if len(s) > PasswordMaxBytes {
		return ErrPasswordTooLong
	}
	if isSingleRepeatedChar(s) {
		return ErrPasswordTrivial
	}
	return nil
}

// isSingleRepeatedChar reports whether s is one byte value repeated, e.g.
// "aaaaaaaa" or "00000000".
func isSingleRepeatedChar(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}
