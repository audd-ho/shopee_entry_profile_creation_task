package validate

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrNicknameEmpty    = errors.New("nickname is empty")
	ErrNicknameTooLong  = errors.New("nickname must be at most 32 characters")
	ErrNicknameBadUTF8  = errors.New("nickname must be valid UTF-8")
)

// Nickname validates and cleans a nickname.
// Returns the cleaned nickname (whitespace trimmed, control chars stripped) and any error.
func Nickname(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrNicknameBadUTF8
	}
	// Strip control characters
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return "", ErrNicknameEmpty
	}
	if utf8.RuneCountInString(cleaned) > 32 {
		return "", ErrNicknameTooLong
	}
	return cleaned, nil
}
