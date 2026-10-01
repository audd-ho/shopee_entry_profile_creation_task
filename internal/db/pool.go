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
//
//	func New(dsn string, maxOpen int) (*Pool, error)
//	func (p *Pool) Acquire(ctx context.Context) (*Conn, error)
//	func (p *Pool) Release(*Conn) — called on Conn.Close()
//	func (p *Pool) Close() error
//	func (p *Pool) Stats() PoolStats — in-use, idle, waiters, waits, wait_time
//
// Metrics to expose:
//
//	in_use, idle, wait_count, wait_duration, evictions
package db

// ---------------------------------------------------------------------------
// Phase 4, job 1: the pool lifecycle. Option C from §4.6.0 — the driver keeps
// the wire protocol and the type conversion, we own acquire, release, sizing,
// health and shutdown.
//
// Capacity is a buffered channel used as a semaphore. Sending into it is
// "taking a slot", receiving is "giving it back", and len(sem) is exactly the
// number of connections in use. Separating the SLOT from the CONNECTION is what
// keeps this simple: a waiter blocks on capacity, not on one specific socket,
// so there is no queue of waiters to hand connections to by hand.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"

	"entry_profile_creation/internal/config"
)

const (
// maxIdleTime retires connections nobody has used in a while, so a pool
// that grew during a spike shrinks back afterwards.
//maxIdleTime = 2 * time.Minute

// sweepInterval is how often the background sweeper looks for stale idle
// connections. Eviction is also checked on checkout; the sweeper exists so
// an idle pool does not hold dead sockets indefinitely.
//sweepInterval = 30 * time.Second

// defaultMaxWait bounds how long Acquire waits for a slot when the pool is
// full, if config does not say.
//defaultMaxWait = 200 * time.Millisecond

// maxBadConnRetries is how many times a query is retried on a fresh
// connection after the driver reports the old one unusable.
//
// Retrying is only safe because driver.ErrBadConn specifically means the
// statement never reached the server. Retrying a generic network error
// could double-apply an INSERT whose response was merely lost.
// maxBadConnRetries = 2
)

var (
	// ErrPoolClosed is returned by Acquire after Close.
	ErrPoolClosed = errors.New("db: pool is closed")

	// ErrPoolExhausted means every connection was busy for longer than the wait
	// timeout. This is deliberate load shedding: a caller queued behind a
	// hundred others still occupies a goroutine and a socket, and answering
	// "busy" in 200ms is more useful than answering correctly in nine seconds.
	ErrPoolExhausted = errors.New("db: pool exhausted, wait timeout exceeded")
)

// PoolStats is what phase 6 tunes against. WaitCount climbing is the signal
// that the pool is too small, as opposed to MySQL simply being slow.
type PoolStats struct {
	InUse        int
	Idle         int
	Opened       int64
	WaitCount    int64
	WaitDuration time.Duration
	Evictions    int64
}

type Pool struct {
	connector driver.Connector
	maxOpen   int

	// sem is the capacity semaphore: cap(sem) == maxOpen, len(sem) == in use.
	sem chan struct{}

	mu     sync.Mutex
	idle   []*Conn // LIFO stack; the top is the most recently used
	closed bool

	opened    atomic.Int64
	waitCount atomic.Int64
	waitNanos atomic.Int64
	evictions atomic.Int64

	stopSweeper chan struct{}
	sweeperDone chan struct{}

	connMaxLifetime   time.Duration
	maxIdleTime       time.Duration
	sweepInterval     time.Duration
	maxWait           time.Duration
	maxBadConnRetries int
}

// New builds a pool from the MySQL config.
//
// It does not connect. Connections are dialled lazily on demand, so a pool of
// 50 does not open 50 sockets at boot for a service that may only ever need
// three. Open in users.go pings once to fail fast on a bad DSN.
func New(cfg *config.MySQLPoolConfig, DSN string) (*Pool, error) {
	myCfg, err := mysql.ParseDSN(DSN)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	connector, err := mysql.NewConnector(myCfg)
	if err != nil {
		return nil, fmt.Errorf("db: build connector: %w", err)
	}

	maxOpen := cfg.MaxOpen
	if maxOpen <= 0 {
		maxOpen = 1
	}

	p := &Pool{
		connector:   connector,
		maxOpen:     maxOpen,
		sem:         make(chan struct{}, maxOpen),
		stopSweeper: make(chan struct{}),
		sweeperDone: make(chan struct{}),

		connMaxLifetime:   cfg.ConnMaxLifetime,
		maxIdleTime:       cfg.MaxIdleTime,
		sweepInterval:     cfg.SweepInterval,
		maxWait:           cfg.MaxWait,
		maxBadConnRetries: cfg.MaxBadConnRetries,
	}

	go p.sweep()
	return p, nil
}

// Acquire checks out a connection for the caller's exclusive use.
//
// Callers must pair every successful Acquire with a Release, or the slot leaks
// and the pool shrinks by one permanently.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	if p.isClosed() {
		return nil, ErrPoolClosed
	}

	// Fast path: a slot is free right now, so nothing is counted as waiting.
	select {
	case p.sem <- struct{}{}:
	default:
		if err := p.waitForSlot(ctx); err != nil {
			return nil, err
		}
	}

	// Close may have happened while we were waiting.
	if p.isClosed() {
		<-p.sem
		return nil, ErrPoolClosed
	}

	if c := p.popIdle(); c != nil {
		return c, nil
	}

	c, err := p.dial(ctx)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return c, nil
}

// waitForSlot blocks until capacity frees up, the caller gives up, or the wait
// timeout fires.
func (p *Pool) waitForSlot(ctx context.Context) error {
	start := time.Now()
	p.waitCount.Add(1)
	defer func() { p.waitNanos.Add(int64(time.Since(start))) }()

	timer := time.NewTimer(p.maxWait)
	defer timer.Stop()

	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		// The client hung up. Giving up here frees the slot for someone whose
		// answer is still wanted.
		return ctx.Err()
	case <-timer.C:
		return ErrPoolExhausted
	}
}

// Release returns a connection to the pool. Connections the driver marked
// broken, and anything released after Close, are destroyed instead.
func (p *Pool) Release(c *Conn) {
	if c == nil {
		return
	}
	c.lastUsed = time.Now()

	discard := c.broken
	if !discard {
		p.mu.Lock()
		if p.closed {
			discard = true
		} else {
			p.idle = append(p.idle, c) // LIFO: pushed and popped from the end
		}
		p.mu.Unlock()
	}

	if discard {
		_ = c.destroy()
		p.evictions.Add(1)
	}

	// Free the slot last, so a waiter never wakes before the connection is
	// actually back in the idle list.
	<-p.sem
}

// popIdle takes the most recently used healthy connection, discarding any
// stale ones it passes.
//
// LIFO rather than FIFO: under light load it concentrates traffic on a few
// connections so the rest go idle long enough for the sweeper to retire them.
// Round-robin would keep every connection barely alive forever.
func (p *Pool) popIdle() *Conn {
	now := time.Now()

	p.mu.Lock()
	var stale []*Conn
	var chosen *Conn
	for len(p.idle) > 0 {
		c := p.idle[len(p.idle)-1]
		p.idle[len(p.idle)-1] = nil
		p.idle = p.idle[:len(p.idle)-1]

		if c.expired(now, p.connMaxLifetime, p.maxIdleTime) {
			stale = append(stale, c)
			continue
		}
		chosen = c
		break
	}
	p.mu.Unlock()

	// Closing sockets is I/O; do it outside the lock.
	for _, c := range stale {
		_ = c.destroy()
		p.evictions.Add(1)
	}
	return chosen
}

func (p *Pool) dial(ctx context.Context) (*Conn, error) {
	raw, err := p.connector.Connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	p.opened.Add(1)
	return newConn(raw), nil
}

func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// sweep retires idle connections in the background.
//
// Without this, MySQL's wait_timeout (8 hours by default) closes sessions
// server-side and the pool keeps handing out sockets that are already dead. The
// retry path would recover, but only after a user request has already failed
// once.
func (p *Pool) sweep() {
	defer close(p.sweeperDone)

	ticker := time.NewTicker(p.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopSweeper:
			return
		case <-ticker.C:
			p.evictStale()
		}
	}
}

func (p *Pool) evictStale() {
	now := time.Now()

	p.mu.Lock()
	keep := make([]*Conn, 0, len(p.idle))
	var dead []*Conn
	for _, c := range p.idle {
		if c.expired(now, p.connMaxLifetime, p.maxIdleTime) {
			dead = append(dead, c)
			continue
		}
		keep = append(keep, c)
	}
	p.idle = keep
	p.mu.Unlock()

	for _, c := range dead {
		_ = c.destroy()
		p.evictions.Add(1)
	}
}

// Close drains the pool and shuts it down.
//
// Draining works by occupying every slot: each send blocks until an in-flight
// caller releases, so once all maxOpen sends complete, nobody holds a
// connection and no new Acquire can succeed.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	close(p.stopSweeper)
	<-p.sweeperDone

	for range p.maxOpen {
		p.sem <- struct{}{}
	}

	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()

	var firstErr error
	for _, c := range idle {
		if err := c.destroy(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Stats reports a snapshot. The counters are read individually, so a busy pool
// can return a very slightly inconsistent view; that is fine for tuning.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	idle := len(p.idle)
	p.mu.Unlock()

	return PoolStats{
		InUse:        len(p.sem),
		Idle:         idle,
		Opened:       p.opened.Load(),
		WaitCount:    p.waitCount.Load(),
		WaitDuration: time.Duration(p.waitNanos.Load()),
		Evictions:    p.evictions.Load(),
	}
}

// --- the surface users.go talks to ----------------------------------------
//
// These deliberately mirror *sql.DB's method names and shapes, so re-pointing
// the Handle alias did not require rewriting the query helpers.

// PingContext checks out a connection and pings it.
func (p *Pool) PingContext(ctx context.Context) error {
	c, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer p.Release(c)

	if err := c.ping(ctx); err != nil {
		c.broken = true
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}

// ExecContext runs a statement that returns no rows.
func (p *Pool) ExecContext(ctx context.Context, query string, args ...any) (driver.Result, error) {
	for attempt := 0; ; attempt++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			return nil, err
		}

		res, err := c.exec(ctx, query, args)
		if isBadConn(err) && attempt < p.maxBadConnRetries {
			c.broken = true
			p.Release(c)
			continue
		}
		p.Release(c)
		return res, err
	}
}

// QueryRowContext runs a query and returns at most one row. It never returns
// nil; any error is carried on the Row and surfaces from Scan, which is how
// *sql.Row behaves and is what lets callers chain the two calls.
func (p *Pool) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	for attempt := 0; ; attempt++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			return &Row{err: err}
		}

		vals, err := c.queryRow(ctx, query, args)
		if isBadConn(err) && attempt < p.maxBadConnRetries {
			c.broken = true
			p.Release(c)
			continue
		}
		p.Release(c)

		if err != nil {
			return &Row{err: err}
		}
		return &Row{vals: vals}
	}
}

// isBadConn reports whether the driver is telling us this connection is
// unusable AND that the statement did not run.
func isBadConn(err error) bool {
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn)
}
