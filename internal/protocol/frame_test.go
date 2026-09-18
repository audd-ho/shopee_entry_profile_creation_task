// TODO: Tests for the frame codec.
//
// Coverage to add:
//   - Round-trip encode/decode of Request and Response
//   - Newline handling in payloads (should reject or escape)
//   - Malformed JSON → returns error, doesn't panic
//   - Empty stream → clean EOF
//   - Very large payload (2MB image upload frame)
package protocol
