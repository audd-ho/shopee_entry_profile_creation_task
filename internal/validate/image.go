package validate

import (
	"errors"
	"net/http"
)

var (
	ErrImageTooLarge = errors.New("image exceeds maximum size")
	ErrImageBadType  = errors.New("image must be JPEG, PNG, or WebP")
)

// MaxImageBytes is the max allowed image upload size (2 MB).
const MaxImageBytes = 2 * 1024 * 1024

var allowedImageMIME = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// Image validates uploaded image bytes.
// Returns the detected canonical MIME type if valid, else error.
func Image(data []byte) (string, error) {
	if len(data) > MaxImageBytes {
		return "", ErrImageTooLarge
	}
	if len(data) == 0 {
		return "", ErrImageBadType
	}
	// http.DetectContentType reads up to 512 bytes internally
	mime := http.DetectContentType(data)
	// Some browsers/tools produce "image/jpeg; charset=..." — strip parameters
	for i, c := range mime {
		if c == ';' {
			mime = mime[:i]
			break
		}
	}
	if !allowedImageMIME[mime] {
		return "", ErrImageBadType
	}
	return mime, nil
}

// ExtensionForMIME returns the canonical file extension for an allowed MIME type.
func ExtensionForMIME(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	}
	return "bin"
}
