// Package validate provides input validation for user-facing fields.
package validate

import (
	"errors"
	"regexp"
)

var (
	ErrUsernameEmpty    = errors.New("username is empty")
	ErrUsernameTooShort = errors.New("username must be at least 3 characters")
	ErrUsernameTooLong  = errors.New("username must be at most 32 characters")
	ErrUsernameBadChars = errors.New("username may only contain letters, digits, and underscore")
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Username validates that the username meets the required format.
// Returns nil if valid.
func Username(s string) error {
	if s == "" {
		return ErrUsernameEmpty
	}
	if len(s) < 3 {
		return ErrUsernameTooShort
	}
	if len(s) > 32 {
		return ErrUsernameTooLong
	}
	if !usernameRe.MatchString(s) {
		return ErrUsernameBadChars
	}
	return nil
}
