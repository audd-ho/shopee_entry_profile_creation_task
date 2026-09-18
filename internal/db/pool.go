// Package db implements a custom MySQL connection pool with per-connection
// prepared statement caching (see §4.6.0 and §4.12 of system_design.md).
//
// Approach: use the go-sql-driver/mysql Connector to obtain individual raw MySQL
// connections and manage lifecycle (checkout/release/health/limits) ourselves.
package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	ErrPoolClosed  = errors.New("pool closed")
	ErrPoolTimeout = errors.New("pool acquire timeout")
)

// Pool is a bounded connection pool with wait queue and health tracking.
type Pool struct {
	connector driver.Connector

	maxOpen int
	maxWait time.Duration

	mu       sync.Mutex
	idle     []*Conn // LIFO stack of idle connections
	openN    int     // count of open connections (in-use + idle)
	waiters  []chan waitResult
	closed   bool
}

type waitResult struct {
	conn *Conn
	err  error
}

// New constructs a pool. The DSN is a standard go-sql-driver/mysql DSN.
func New(dsn string, maxOpen int, maxWait time.Duration) (*Pool, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return &Pool{
		connector: connector,
		maxOpen:   maxOpen,
		maxWait:   maxWait,
	}, nil
}

// Acquire returns a connection from the pool, opening a new one if under limit,
// otherwise waiting up to maxWait for a released connection.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}

	// 1. Try idle stack (LIFO — most-recently-released is warmest)
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return c, nil
	}

	// 2. Open a new connection if under the cap
	if p.openN < p.maxOpen {
		p.openN++
		p.mu.Unlock()
		c, err := p.dial(ctx)
		if err != nil {
			// Failed to open; give back the slot
			p.mu.Lock()
			p.openN--
			p.mu.Unlock()
			return nil, err
		}
		return c, nil
	}

	// 3. Pool is saturated — wait for a release
	ch := make(chan waitResult, 1)
	p.waiters = append(p.waiters, ch)
	p.mu.Unlock()

	timer := time.NewTimer(p.maxWait)
	defer timer.Stop()

	select {
	case r := <-ch:
		return r.conn, r.err
	case <-timer.C:
		p.removeWaiter(ch)
		return nil, ErrPoolTimeout
	case <-ctx.Done():
		p.removeWaiter(ch)
		return nil, ctx.Err()
	}
}

func (p *Pool) removeWaiter(ch chan waitResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, w := range p.waiters {
		if w == ch {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			return
		}
	}
}

// Release returns a connection to the pool.
// If a waiter is queued, hand it over directly.
// If the connection is unhealthy, discard it (openN decrements, freeing a slot).
func (p *Pool) Release(c *Conn, healthy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed || !healthy {
		p.openN--
		go c.rawClose()
		// If waiters are queued, they need a fresh open — signal ErrPoolClosed only when closed.
		if p.closed && len(p.waiters) > 0 {
			for _, w := range p.waiters {
				w <- waitResult{err: ErrPoolClosed}
			}
			p.waiters = nil
		}
		return
	}

	// Hand off to a waiter if any
	if len(p.waiters) > 0 {
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		w <- waitResult{conn: c}
		return
	}

	// Otherwise return to idle stack
	c.lastUsed = time.Now()
	p.idle = append(p.idle, c)
}

// Close closes all idle connections and prevents new acquisitions.
// In-use connections are closed on their next Release.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	waiters := p.waiters
	p.waiters = nil
	p.mu.Unlock()

	for _, c := range idle {
		c.rawClose()
	}
	for _, w := range waiters {
		w <- waitResult{err: ErrPoolClosed}
	}
	return nil
}

// dial opens a new MySQL connection.
func (p *Pool) dial(ctx context.Context) (*Conn, error) {
	raw, err := p.connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &Conn{
		raw:       raw,
		pool:      p,
		stmtCache: make(map[string]driver.Stmt),
		created:   time.Now(),
		lastUsed:  time.Now(),
	}, nil
}

// Stats reports pool state (for perf tuning / observability).
type Stats struct {
	MaxOpen    int
	OpenNow    int
	Idle       int
	InUse      int
	WaitingNow int
}

func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		MaxOpen:    p.maxOpen,
		OpenNow:    p.openN,
		Idle:       len(p.idle),
		InUse:      p.openN - len(p.idle),
		WaitingNow: len(p.waiters),
	}
}
