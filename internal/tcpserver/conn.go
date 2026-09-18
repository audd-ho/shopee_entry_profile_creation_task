// TODO: Per-TCP-connection handler on the server side.
//
// Responsibilities:
//   - Read frames from the connection in a loop (protocol.Decode)
//   - For each frame: dispatch to the appropriate handler (see dispatch.go)
//   - Write response frames back over the same connection
//     (protect the write side with a mutex — multiple handler goroutines write concurrently)
//   - Support concurrent request handling per connection (goroutine per request)
//   - Handle connection close, malformed frames, panics
//
// Flow:
//   for {
//       req := protocol.Decode(conn)
//       go func() {
//           resp := dispatch(req)
//           writeMu.Lock(); protocol.Encode(conn, resp); writeMu.Unlock()
//       }()
//   }
package tcpserver
