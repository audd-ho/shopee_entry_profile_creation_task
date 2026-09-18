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
