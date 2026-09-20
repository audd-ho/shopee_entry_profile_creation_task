package validate

import (
	"errors"
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"typical", "correct horse battery", nil},
		{"exactly minimum", strings.Repeat("a", PasswordMinBytes-1) + "b", nil},
		{"exactly maximum", strings.Repeat("a", PasswordMaxBytes-1) + "b", nil},
		{"no composition rules", "aaaaaaaaaaaaaaaab", nil},

		{"empty", "", ErrPasswordTooShort},
		{"one short", strings.Repeat("a", PasswordMinBytes-1), ErrPasswordTooShort},
		{"one long", strings.Repeat("a", PasswordMaxBytes) + "b", ErrPasswordTooLong},

		{"all same letter", strings.Repeat("a", 20), ErrPasswordTrivial},
		{"all same digit", "00000000", ErrPasswordTrivial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Password(tt.in); !errors.Is(got, tt.want) {
				t.Errorf("Password(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestPasswordLimitIsBytes guards the subtlety that bcrypt's 72 is a byte
// limit: an emoji costs four bytes, so a password well under 72 CHARACTERS can
// still be over the limit and must be rejected here rather than failing later
// inside bcrypt with an opaque error.
func TestPasswordLimitIsBytes(t *testing.T) {
	emoji := strings.Repeat("🦊", 18) // 18 runes, 72 bytes
	if len(emoji) != PasswordMaxBytes {
		t.Fatalf("test setup: %d bytes, expected %d", len(emoji), PasswordMaxBytes)
	}
	if err := Password(emoji); err != nil {
		t.Errorf("Password(72 bytes of emoji) = %v, want nil", err)
	}
	if err := Password(emoji + "🦊"); !errors.Is(err, ErrPasswordTooLong) {
		t.Errorf("Password(76 bytes) = %v, want ErrPasswordTooLong", err)
	}
}
