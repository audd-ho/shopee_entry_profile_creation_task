package validate

import (
	"bytes"
	"errors"
	"testing"
)

// Minimal byte prefixes that http.DetectContentType recognises. Only the magic
// numbers matter; sniffing never looks past the first 512 bytes.
var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	jpegBytes = append([]byte("\xff\xd8\xff\xe0"), bytes.Repeat([]byte{0}, 32)...)
	webpBytes = append([]byte("RIFF\x00\x00\x00\x00WEBPVP"), bytes.Repeat([]byte{0}, 32)...)
	gifBytes  = append([]byte("GIF89a"), bytes.Repeat([]byte{0}, 32)...)
	htmlBytes = []byte("<html><script>alert(1)</script></html>")
)

func TestImageAccepts(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"png", pngBytes, "image/png"},
		{"jpeg", jpegBytes, "image/jpeg"},
		{"webp", webpBytes, "image/webp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Image(tt.data)
			if err != nil {
				t.Fatalf("Image: %v", err)
			}
			if got != tt.want {
				t.Errorf("mime = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestImageRejects(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrImageEmpty},
		{"too large", bytes.Repeat([]byte{0x41}, MaxImageBytes+1), ErrImageTooLarge},
		// GIF is a real image format and still rejected: the allowlist is three
		// formats, not "anything that looks like a picture".
		{"gif not on allowlist", gifBytes, ErrImageType},
		{"plain text", []byte("just some text, honestly"), ErrImageType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Image(tt.data); !errors.Is(err, tt.want) {
				t.Errorf("Image() error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestImageIgnoresClaimedType is the attack this function exists to stop: an
// HTML document uploaded as avatar.jpg. Sniffing the bytes catches it no matter
// what the filename or Content-Type header claimed.
func TestImageIgnoresClaimedType(t *testing.T) {
	if _, err := Image(htmlBytes); !errors.Is(err, ErrImageType) {
		t.Errorf("Image(html) error = %v, want ErrImageType", err)
	}
}

// TestImageSniffsOnlyTheHead confirms that a valid header followed by arbitrary
// payload still passes. Sniffing proves how the file STARTS, not that the whole
// file is a well-formed image — decoding would be needed for that.
func TestImageSniffsOnlyTheHead(t *testing.T) {
	data := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte("garbage"), 100)...)
	got, err := Image(data)
	if err != nil || got != "image/png" {
		t.Errorf("Image() = %q, %v; want image/png, nil", got, err)
	}
}
