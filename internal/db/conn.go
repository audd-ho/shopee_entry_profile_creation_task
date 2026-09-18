package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"
)

// Conn is a pooled MySQL connection with a per-connection prepared statement cache.
type Conn struct {
	raw       driver.Conn
	pool      *Pool
	stmtCache map[string]driver.Stmt
	created   time.Time
	lastUsed  time.Time

	// broken indicates the connection encountered an unrecoverable error
	// and should not be returned to the pool.
	broken bool
}

// Release returns the connection to the pool.
// Call this exactly once per Acquire.
func (c *Conn) Release() {
	if c == nil || c.pool == nil {
		return
	}
	c.pool.Release(c, !c.broken)
}

// Ping issues a lightweight query to verify the connection is alive.
func (c *Conn) Ping(ctx context.Context) error {
	if p, ok := c.raw.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	// Fallback: run a trivial query
	rows, err := c.Query(ctx, "SELECT 1", nil)
	if err != nil {
		return err
	}
	rows.Close()
	return nil
}

// prepare returns a cached prepared statement, preparing it on first use.
func (c *Conn) prepare(query string) (driver.Stmt, error) {
	if s, ok := c.stmtCache[query]; ok {
		return s, nil
	}
	s, err := c.raw.Prepare(query)
	if err != nil {
		c.broken = true
		return nil, err
	}
	c.stmtCache[query] = s
	return s, nil
}

// Exec runs a query that does not return rows.
func (c *Conn) Exec(ctx context.Context, query string, args []driver.Value) (driver.Result, error) {
	s, err := c.prepare(query)
	if err != nil {
		return nil, err
	}
	if execer, ok := s.(driver.StmtExecContext); ok {
		return execer.ExecContext(ctx, valuesToNamed(args))
	}
	return s.Exec(args) //nolint:staticcheck
}

// Query runs a query that returns rows.
func (c *Conn) Query(ctx context.Context, query string, args []driver.Value) (driver.Rows, error) {
	s, err := c.prepare(query)
	if err != nil {
		return nil, err
	}
	if qer, ok := s.(driver.StmtQueryContext); ok {
		return qer.QueryContext(ctx, valuesToNamed(args))
	}
	return s.Query(args) //nolint:staticcheck
}

// ErrNoRows is returned when a query expected a row but got none.
var ErrNoRows = errors.New("db: no rows")

// rawClose closes underlying resources. Called by the pool.
func (c *Conn) rawClose() {
	for _, s := range c.stmtCache {
		_ = s.Close()
	}
	c.stmtCache = nil
	if c.raw != nil {
		_ = c.raw.Close()
		c.raw = nil
	}
}

func valuesToNamed(vs []driver.Value) []driver.NamedValue {
	nvs := make([]driver.NamedValue, len(vs))
	for i, v := range vs {
		nvs[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return nvs
}
