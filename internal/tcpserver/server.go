// Package tcpserver implements the business-logic backend that speaks the
// custom JSON-lines protocol over TCP.
package tcpserver

import (
	"context"
	"log"
	"net"
	"sync"

	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/db"
)

// Server is the TCP business-logic server.
type Server struct {
	cfg   *config.Config
	pool  *db.Pool
	cache *cache.Client

	listener net.Listener
	wg       sync.WaitGroup
}

// New constructs a Server.
func New(cfg *config.Config, pool *db.Pool, c *cache.Client) *Server {
	return &Server{cfg: cfg, pool: pool, cache: c}
}

// Run starts the accept loop and blocks until ctx is done or Listen fails.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.TCP.Addr)
	if err != nil {
		return err
	}
	s.listener = ln
	log.Printf("tcp-server listening on %s", s.cfg.TCP.Addr)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed or fatal — exit
			if ctx.Err() != nil {
				break
			}
			log.Printf("accept error: %v", err)
			continue
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.handleConn(ctx, c)
		}(conn)
	}
	s.wg.Wait()
	return nil
}
