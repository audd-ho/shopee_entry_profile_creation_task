// TODO: Nickname validation.
//
// Rules (see §4.10):
//   - Valid UTF-8
//   - 1-32 RUNES (not bytes) — use utf8.RuneCountInString
//   - Strip control chars, zero-width chars, RTL overrides
//   - Full unicode allowed (emojis, CJK, etc.)
//
// Public API:
//   func Nickname(s string) (cleaned string, err error)
//     Returns cleaned nickname (whitespace trimmed, control chars stripped) and error if invalid.
package validate
