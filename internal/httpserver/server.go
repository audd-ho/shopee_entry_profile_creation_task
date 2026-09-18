// Package httpserver implements the HTTP frontend that speaks HTTP to browsers
// and relays requests to the TCP backend server over a pooled, multiplexed
// TCP connection.
package httpserver

import (
	"context"
	"log"
	"net/http"
	"time"

	"entry_profile_creation/internal/config"
)

// Server holds the HTTP server and its dependencies.
type Server struct {
	cfg    *config.Config
	tcpCli *TCPClient
	http   *http.Server
}

// New constructs an HTTP Server.
func New(cfg *config.Config) (*Server, error) {
	tcpCli, err := NewTCPClient(cfg.HTTP.TCPAddr, cfg.HTTP.TCPPoolSize)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, tcpCli: tcpCli}
	s.http = &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           s.buildRouter(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// Run starts the HTTP server and blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	log.Printf("http-server listening on %s (backend: %s)", s.cfg.HTTP.Addr, s.cfg.HTTP.TCPAddr)
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
		_ = s.tcpCli.Close()
		return nil
	}
}
