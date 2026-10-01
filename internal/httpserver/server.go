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
//
//	func New(cfg *config.Config) (*Server, error)
//	func (s *Server) Run(ctx context.Context) error
package httpserver

import (
	"context"
	"entry_profile_creation/internal/config"
	"log"
	"net/http"
	"time"
)

type Server struct {
	cfg           *config.Config
	tcpClientPool *TCPConnectionPool
	http          *http.Server
}

func NewServer(cfg *config.Config) (*Server, error) {
	tcpClientPool := NewTCPConnectionPool(cfg.TCP.Addr, cfg.HTTP.TCPPoolSize)
	// no error since if got then logs? not too sure?
	s := &Server{cfg: cfg, tcpClientPool: tcpClientPool}
	s.http = &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           s.buildRouter(),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}
	return s, nil
}

// Run starts the HTTP server and blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	log.Printf("http-server listening on %s (backend: %s)", s.cfg.HTTP.Addr, s.cfg.TCP.Addr)
	errCh := make(chan error, 1)
	go func() {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
		_ = s.tcpClientPool.Close()
		return nil
	}
}
