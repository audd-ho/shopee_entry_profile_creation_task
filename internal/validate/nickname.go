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

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	NicknameMinRunes = 1
	NicknameMaxRunes = 32
)

var (
	ErrNicknameInvalidUTF8 = errors.New("nickname is not valid UTF-8")
	ErrNicknameEmpty       = errors.New("nickname must not be empty")
	ErrNicknameTooLong     = fmt.Errorf("nickname must be at most %d characters", NicknameMaxRunes)
)

// Nickname cleans and validates a display name, returning the cleaned form.
//
// Unlike Username this allows the whole of unicode — emoji, CJK, accents — and
// counts RUNES rather than bytes, so "日本語の名前" is 6 characters and not 18.
// Counting bytes would give unicode users a fraction of the allowance ASCII
// users get.
//
// Stripped rather than rejected:
//   - control characters, which can corrupt terminal output and log lines
//   - unicode format characters (Cf), which covers zero-width joiners and
//     spaces used to fake distinct names out of identical-looking strings, and
//     the bidirectional overrides U+202A-U+202E and U+2066-U+2069 that can make
//     text render in an order unrelated to the bytes stored
//
// Stripping happens before trimming and before counting, so " ‮admin " is
// measured as the "admin" it will actually display as.
func Nickname(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrNicknameInvalidUTF8
	}

	cleaned := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return -1
		case unicode.Is(unicode.Cf, r):
			return -1
		default:
			return r
		}
	}, s)

	cleaned = strings.TrimSpace(cleaned)

	switch n := utf8.RuneCountInString(cleaned); {
	case n < NicknameMinRunes:
		return "", ErrNicknameEmpty
	case n > NicknameMaxRunes:
		return "", ErrNicknameTooLong
	}
	return cleaned, nil
}
