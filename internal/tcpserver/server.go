// TODO: TCP server lifecycle.
//
// Responsibilities:
//   - net.Listen("tcp", addr) — accept loop
//   - For each accepted connection: spawn a goroutine running conn.go:HandleConn
//   - Own the shared dependencies (db pool, redis client, config) — pass to handlers
//   - Graceful shutdown: close listener, wait for in-flight handlers to drain
//
// Public API:
//
//	func New(cfg *config.Config, pool *db.Pool, cache *cache.Client) (*Server, error)
//	func (s *Server) Run(ctx context.Context) error
package tcpserver

import (
	"context"
	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/db"
	"log"
	"net"
	"sync"
)

type Server struct {
	cfg *config.Config

	// db is a *sql.DB for now. In phase 4 db.Handle is re-pointed at the custom
	// pool and nothing in this package changes.
	db    db.Handle
	cache *cache.Client

	listener net.Listener
	wg       sync.WaitGroup
}

// NewServer takes its dependencies rather than building them, so main owns the
// lifetime of the database and Redis connections and the handlers stay testable
// against substitutes.
func NewServer(cfg *config.Config, dbh db.Handle, redis *cache.Client) *Server {
	return &Server{cfg: cfg, db: dbh, cache: redis}
}

func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.TCP.Addr)
	if err != nil {
		return err
	}

	s.listener = listener
	log.Printf("tcp-server listening on %s", s.cfg.TCP.Addr)

	// close listener when server context dones(closed, so can run sinc empty+closed), closes
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		connection, err := listener.Accept()
		if err != nil {
			// Listener closed or fatal — exit
			if ctx.Err() != nil { // server listening ended then break, else log accept error and continue
				break
			}
			log.Printf("accept error: %v", err)
			continue
		}

		s.wg.Add(1) // connections accepted

		go func(conn net.Conn) {
			defer s.wg.Done()
			s.handleConnection(ctx, conn)
		}(connection)
	}

	s.wg.Wait() // all connections closed before returning
	return nil
}
