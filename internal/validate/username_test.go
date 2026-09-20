package validate

import (
	"errors"
	"strings"
	"testing"
)

func TestUsername(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"typical", "alice_99", nil},
		{"minimum length", "abc", nil},
		{"maximum length", strings.Repeat("a", UsernameMaxLen), nil},
		{"all underscores", "___", nil},

		{"empty", "", ErrUsernameLength},
		{"one short", "ab", ErrUsernameLength},
		{"one long", strings.Repeat("a", UsernameMaxLen+1), ErrUsernameLength},

		{"space", "alice smith", ErrUsernameChars},
		{"hyphen", "alice-smith", ErrUsernameChars},
		{"at sign", "alice@example.com", ErrUsernameChars},
		{"emoji", "alice🦊x", ErrUsernameChars},
		// Cyrillic 'а' renders identically to Latin 'a'. Allowing it would let
		// someone register a visually identical impersonation of an account.
		{"cyrillic lookalike", "аlice", ErrUsernameChars},
		{"newline injection", "alice\nadmin", ErrUsernameChars},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Username(tt.in); !errors.Is(got, tt.want) {
				t.Errorf("Username(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestUsernameIsNotCaseFolded pins the decision not to normalise case, which
// has to agree with the utf8mb4_bin collation on uk_username.
func TestUsernameIsNotCaseFolded(t *testing.T) {
	for _, s := range []string{"Alice", "ALICE", "alice"} {
		if err := Username(s); err != nil {
			t.Errorf("Username(%q) = %v, want nil", s, err)
		}
	}
}
