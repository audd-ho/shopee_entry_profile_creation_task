// TODO: Persistent multiplexed TCP client pool (HTTP server → TCP server).
//
// Design (see §4.7 of system_design.md):
//   - Maintain N (8-16) persistent TCP connections to the TCP server
//   - Multiplex concurrent requests over each connection via req_id
//   - One reader goroutine per connection; matches responses to waiters by req_id
//   - Write side: serialize with per-connection mutex OR channel of outgoing frames
//
// Data structures:
//   type Pool struct { conns []*muxConn; roundRobinIdx atomic.Uint64 }
//   type muxConn struct {
//       conn      net.Conn
//       writeMu   sync.Mutex
//       nextID    atomic.Uint64
//       pending   map[uint64]chan protocol.Response
//       pendingMu sync.Mutex
//       closed    chan struct{}
//   }
//
// Public API:
//   func NewPool(addr string, size int) (*Pool, error)
//   func (p *Pool) Do(ctx context.Context, req protocol.Request) (protocol.Response, error)
//   func (p *Pool) Close() error
//
// Timeouts:
//   - Per-request: 2s (context deadline)
//   - Reconnect on connection death (drop pending, return error to callers)
package httpserver
