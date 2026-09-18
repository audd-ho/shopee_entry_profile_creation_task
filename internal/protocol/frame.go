// TODO: HTTP↔TCP wire protocol framing.
//
// Framing: newline-delimited JSON (v1).
//   Request:  {"req_id": <int>, "op": "<string>", "data": {...}}\n
//   Response: {"req_id": <int>, "status": "ok"|"err", "error": "...", "data": {...}}\n
//
// Responsibilities:
//   - Define Request and Response struct types with json tags
//   - Provide Encode(w io.Writer, msg) and Decode(r io.Reader) helpers
//   - Use bufio.Scanner or bufio.Reader with '\n' delimiter for read side
//   - Ensure single-line JSON (no pretty-print) so newline is unambiguous
//
// Future (stretch): swap for length-prefixed binary framing.
// Shared by both cmd/http-server (client side) and cmd/tcp-server (server side).
package protocol
