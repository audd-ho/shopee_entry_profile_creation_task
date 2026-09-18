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
