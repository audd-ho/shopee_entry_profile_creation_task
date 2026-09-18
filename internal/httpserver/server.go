// TODO: HTTP server lifecycle.
//
// Responsibilities:
//   - Construct *http.Server with configured addr, timeouts, handler
//   - Register routes (see routes.go)
//   - Attach middleware chain (see middleware.go)
//   - Wire up the TCP client pool (see tcpclient.go)
//   - Serve static frontend files from ./web (or ./frontend, depending on final path)
//   - Graceful shutdown: on context cancel, http.Server.Shutdown() with a timeout
//
// Public API:
//   func New(cfg *config.Config) (*Server, error)
//   func (s *Server) Run(ctx context.Context) error
package httpserver
