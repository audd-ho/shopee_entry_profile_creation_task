// TODO: Username validation.
//
// Rules (see §4.10):
//   - Regex: ^[a-zA-Z0-9_]{3,32}$
//   - ASCII only
//   - Optionally lowercase-normalize before storage/comparison
//
// Public API:
//   func Username(s string) error
//     Returns nil if valid, else a specific error (ErrTooShort, ErrTooLong, ErrBadChars).
package validate

import (
	"errors"
	"fmt"
	"regexp"
)

const (
	UsernameMinLen = 3
	UsernameMaxLen = 32
)

var (
	ErrUsernameLength = fmt.Errorf("username must be %d-%d characters", UsernameMinLen, UsernameMaxLen)
	ErrUsernameChars  = errors.New("username may contain only letters, digits and underscore")
)

// Compiled once at package init; compiling per call would put a regex compile
// on the signup path.
var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Username reports whether s is an acceptable username.
//
// Deliberately no lowercase normalisation. The users table is utf8mb4_bin, so
// Alice and alice are distinct accounts by design; folding case here would
// silently contradict the schema and make uk_username reject signups that the
// database would have accepted.
//
// The ASCII-only restriction is not squeamishness about unicode — nicknames are
// full unicode. It is that usernames are an identity primitive, and confusable
// characters (Cyrillic а vs Latin a) make impersonation trivial.
func Username(s string) error {
	// Byte length is safe as a rune count here only because the regex below
	// rejects anything non-ASCII.
	if len(s) < UsernameMinLen || len(s) > UsernameMaxLen {
		return ErrUsernameLength
	}
	if !usernameRe.MatchString(s) {
		return ErrUsernameChars
	}
	return nil
}
