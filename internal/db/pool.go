// TODO: Custom MySQL connection pool (see §4.6.0 of system_design.md).
//
// Approach (Option C): use go-sql-driver/mysql's Connector to obtain raw MySQL
// connections, and manage the pool lifecycle ourselves.
//
// Responsibilities:
//   - Bounded max open connections (start 50, tune during perf)
//   - Idle connection reuse (LIFO or FIFO; start with LIFO — warmer connections)
//   - Wait queue with timeout (~200ms) — return error under overload rather than block
//   - Health check / stale eviction: ping every N min, evict max_lifetime exceeded
//   - Per-connection prepared statement cache (see conn.go)
//   - Graceful shutdown: drain in-flight, close idle
//
// Public API:
//   func New(dsn string, maxOpen int) (*Pool, error)
//   func (p *Pool) Acquire(ctx context.Context) (*Conn, error)
//   func (p *Pool) Release(*Conn) — called on Conn.Close()
//   func (p *Pool) Close() error
//   func (p *Pool) Stats() PoolStats — in-use, idle, waiters, waits, wait_time
//
// Metrics to expose:
//   in_use, idle, wait_count, wait_duration, evictions
package db
