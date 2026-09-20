// TODO: Image upload validation (see §4.4.1).
//
// Rules:
//   - Max size: 2 MB (enforced via http.MaxBytesReader before full read)
//   - MIME sniff via http.DetectContentType on first 512 bytes
//   - Allowlist: image/jpeg, image/png, image/webp
//   - Stretch: image.Decode for dimension check (reject > 4096×4096)
//
// Public API:
//   func Image(data []byte) (mimeType string, err error)
//     Returns detected MIME type if valid, else error.
package validate

import (
	"errors"
	"fmt"
	"net/http"
)

// MaxImageBytes matches config.Uploads.MaxBytes. The HTTP layer should still
// cap the request body with http.MaxBytesReader so an oversized upload is
// rejected while streaming, rather than after buffering it all in memory; this
// constant is the second line of defence for callers that already hold bytes.
const MaxImageBytes = 2 << 20 // 2 MiB

// sniffLen is the window http.DetectContentType inspects. Passing more is
// wasted work; passing less can miss a signature.
const sniffLen = 512

var allowedImageTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

var (
	ErrImageEmpty    = errors.New("image is empty")
	ErrImageTooLarge = fmt.Errorf("image must be at most %d bytes", MaxImageBytes)
	ErrImageType     = errors.New("unsupported image type")
)

// Image validates raw upload bytes and returns the detected MIME type.
//
// The type comes from sniffing the actual bytes, never from the filename or the
// client's Content-Type header, both of which are attacker-controlled. The
// attack it blocks is concrete: upload an HTML document named avatar.jpg, get
// it served back from our origin, and any script in it runs with our cookies.
// An allowlist of three raster formats plus the X-Content-Type-Options: nosniff
// header on /uploads/ closes that off.
//
// Note that sniffing proves the file STARTS like a JPEG, not that it is a
// well-formed one. Decoding it to confirm (and to bound the dimensions) is the
// stretch item in the TODO above.
func Image(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrImageEmpty
	}
	if len(data) > MaxImageBytes {
		return "", ErrImageTooLarge
	}

	head := data
	if len(head) > sniffLen {
		head = head[:sniffLen]
	}

	mimeType := http.DetectContentType(head)
	if !allowedImageTypes[mimeType] {
		return "", fmt.Errorf("%w: %s", ErrImageType, mimeType)
	}
	return mimeType, nil
}
