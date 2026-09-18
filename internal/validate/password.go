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
