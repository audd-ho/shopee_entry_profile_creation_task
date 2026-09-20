package validate

import (
	"errors"
	"strings"
	"testing"
)

func TestNickname(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{"typical", "Alice", "Alice", nil},
		{"trims surrounding whitespace", "   Alice   ", "Alice", nil},
		{"keeps interior spaces", "Alice B. Smith", "Alice B. Smith", nil},
		{"emoji allowed", "Alice 🦊", "Alice 🦊", nil},
		{"cjk allowed", "日本語の名前", "日本語の名前", nil},

		// 32 runes of CJK is 96 bytes. Counting bytes would reject this while
		// allowing 32 ASCII characters, giving unicode users a third of the
		// allowance.
		{"32 runes of cjk", strings.Repeat("名", 32), strings.Repeat("名", 32), nil},
		{"33 runes", strings.Repeat("a", 33), "", ErrNicknameTooLong},

		{"empty", "", "", ErrNicknameEmpty},
		{"only whitespace", "     ", "", ErrNicknameEmpty},
		{"only control chars", "\x00\x01\x02", "", ErrNicknameEmpty},

		{"strips control chars", "Ali\x07ce", "Alice", nil},
		{"strips newline", "Alice\nadmin", "Aliceadmin", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Nickname(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Nickname(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Nickname(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNicknameStripsInvisibles verifies the claim that unicode.Cf covers the
// characters that matter here. Zero-width characters let two accounts display
// identically, and the bidi overrides can make stored text render in an order
// unrelated to its bytes — "admin\u202Egnp.exe" displays as "adminexe.png".
func TestNicknameStripsInvisibles(t *testing.T) {
	invisibles := map[string]string{
		"zero width space":       "\u200B",
		"zero width non-joiner":  "\u200C",
		"zero width joiner":      "\u200D",
		"left-to-right mark":     "\u200E",
		"right-to-left mark":     "\u200F",
		"right-to-left override": "\u202E",
		"left-to-right isolate":  "\u2066",
		"byte order mark":        "\uFEFF",
	}
	for name, ch := range invisibles {
		t.Run(name, func(t *testing.T) {
			got, err := Nickname("ad" + ch + "min")
			if err != nil {
				t.Fatalf("Nickname: %v", err)
			}
			if got != "admin" {
				t.Errorf("got %q (%U), want %q; the character survived cleaning", got, []rune(got), "admin")
			}
		})
	}
}

// TestNicknameCleansBeforeCounting checks the ordering: a name made entirely of
// invisible characters must count as empty, not as a valid 5-character name.
func TestNicknameCleansBeforeCounting(t *testing.T) {
	if _, err := Nickname("\u200B\u200B\u200B"); !errors.Is(err, ErrNicknameEmpty) {
		t.Errorf("error = %v, want ErrNicknameEmpty", err)
	}
}
