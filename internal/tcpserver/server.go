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
	"log"
	"net"
	"strconv"
	"sync"

	"entry_profile_creation/internal/cache"
	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/idgen"
	"entry_profile_creation/internal/lockstack"
)

type Server struct {
	cfg *config.Config

	// db holds pools to every data shard and lookup shard, plus the consistent
	// hash ring for routing. Handlers call db.DataShardFor / db.LookupShardFor
	// to pick the right pool before calling query helpers.
	db        *db.ShardedHandle
	cache     *cache.Client
	lockstack *lockstack.LockStack
	idgen     *idgen.Generator

	listener net.Listener
	wg       sync.WaitGroup
}

// NewServer takes its dependencies rather than building them, so main owns the
// lifetime of the database and Redis connections and the handlers stay testable
// against substitutes.
func NewServer(cfg *config.Config, dbh *db.ShardedHandle, redis *cache.Client, ls *lockstack.LockStack, gen *idgen.Generator) *Server {
	return &Server{cfg: cfg, db: dbh, cache: redis, lockstack: ls, idgen: gen}
}

func userCacheKey(userID uint64) string {
	return "user:" + strconv.FormatUint(userID, 10)
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
