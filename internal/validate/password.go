package validate

import "errors"

var (
	ErrPasswordTooShort = errors.New("password must be at least 8 characters")
	ErrPasswordTooLong  = errors.New("password must be at most 72 characters")
	ErrPasswordTrivial  = errors.New("password is too simple")
)

// Password validates password length and rejects trivially-weak passwords.
// bcrypt truncates at 72 bytes; we enforce that as the hard max.
func Password(s string) error {
	if len(s) < 8 {
		return ErrPasswordTooShort
	}
	if len(s) > 72 {
		return ErrPasswordTooLong
	}
	// Reject all-same-char
	allSame := true
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			allSame = false
			break
		}
	}
	if allSame {
		return ErrPasswordTrivial
	}
	return nil
}
