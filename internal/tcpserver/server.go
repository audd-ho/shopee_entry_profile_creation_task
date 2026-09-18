// TODO: TCP server lifecycle.
//
// Responsibilities:
//   - net.Listen("tcp", addr) — accept loop
//   - For each accepted connection: spawn a goroutine running conn.go:HandleConn
//   - Own the shared dependencies (db pool, redis client, config) — pass to handlers
//   - Graceful shutdown: close listener, wait for in-flight handlers to drain
//
// Public API:
//   func New(cfg *config.Config, pool *db.Pool, cache *cache.Client) (*Server, error)
//   func (s *Server) Run(ctx context.Context) error
package tcpserver
