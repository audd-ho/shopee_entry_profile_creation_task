// TODO: Per-connection wrapper with prepared statement cache (see §4.12).
//
// Each pooled MySQL connection owns its own cache of prepared statements,
// because MySQL prepares are per-connection.
//
// Structure:
//   type Conn struct {
//       raw       driver.Conn                 // from mysql.Connector
//       stmtCache map[string]*driver.Stmt     // SQL string → prepared stmt
//       cacheMu   sync.Mutex
//       lastUsed  time.Time
//       created   time.Time
//   }
//
// Public API:
//   func (c *Conn) Prepare(sql string) (*driver.Stmt, error)  // uses cache
//   func (c *Conn) Exec(sql string, args ...) (driver.Result, error)
//   func (c *Conn) Query(sql string, args ...) (driver.Rows, error)
//   func (c *Conn) Close() error   // returns to pool, not physically closed
//   func (c *Conn) Ping() error    // health check
//
// Prepared statements to expect (from §4.12):
//   - SELECT id, password_hash, nickname, avatar_version, avatar_suffix FROM users WHERE username = ?
//   - SELECT id, nickname, avatar_version, avatar_suffix FROM users WHERE id = ?
//   - UPDATE users SET nickname = ? WHERE id = ?
//   - UPDATE users SET avatar_version = LAST_INSERT_ID(avatar_version + 1), avatar_suffix = ? WHERE id = ?
//   - INSERT INTO users (username, password_hash, nickname) VALUES (?, ?, ?)
package db

// ---------------------------------------------------------------------------
// Phase 4. This file holds two of the three jobs:
//
//   (2) the database/sql replacement — Row, Scan, driver.Value conversion
//   (3) the per-connection prepared statement cache
//
// The pool lifecycle itself (job 1) is in pool.go.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// ErrNoRows replaces sql.ErrNoRows. Dropping database/sql means dropping its
// sentinel too, and users.go translates this into ErrUserNotFound so callers
// never see either.
var ErrNoRows = errors.New("db: no rows in result set")

// Conn is one exclusively-owned MySQL connection plus its statement cache.
//
// There is no mutex on this struct. The pool hands a Conn to exactly one
// goroutine at a time and does not touch it again until Release, which is also
// what the driver requires: driver.Conn is documented as "only used by one
// goroutine at a time". Adding a mutex here would paper over a violation of
// that contract rather than fix it.
type Conn struct {
	raw driver.Conn

	// stmtCache maps SQL text to a prepared statement handle.
	//
	// It lives on the Conn and not on the Pool because MySQL's COM_STMT_PREPARE
	// returns a handle scoped to that session. A handle from one connection is
	// meaningless on another, so a pool-wide cache would be actively wrong.
	stmtCache map[string]driver.Stmt

	created  time.Time
	lastUsed time.Time

	// broken marks a connection the driver has told us is unusable. Release
	// destroys it instead of returning it to the idle list.
	broken bool
}

func newConn(raw driver.Conn) *Conn {
	now := time.Now()
	return &Conn{
		raw:       raw,
		stmtCache: make(map[string]driver.Stmt),
		created:   now,
		lastUsed:  now,
	}
}

// prepare returns a statement for query, preparing it on first use.
//
// Prepare-on-first-use rather than preparing everything at connect time: the
// eager version would add one round trip per statement to every new connection,
// which is paid again on every reconnect and every pool growth.
func (c *Conn) prepare(ctx context.Context, query string) (driver.Stmt, error) {
	if stmt, ok := c.stmtCache[query]; ok {
		return stmt, nil
	}

	preparer, ok := c.raw.(driver.ConnPrepareContext)
	if !ok {
		return nil, errors.New("db: driver connection does not support PrepareContext")
	}

	stmt, err := preparer.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}

	c.stmtCache[query] = stmt
	return stmt, nil
}

// exec runs a statement that returns no rows.
func (c *Conn) exec(ctx context.Context, query string, args []any) (driver.Result, error) {
	stmt, err := c.prepare(ctx, query)
	if err != nil {
		return nil, err
	}

	execer, ok := stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("db: driver statement does not support ExecContext")
	}

	named, err := toNamedValues(args)
	if err != nil {
		return nil, err
	}
	return execer.ExecContext(ctx, named)
}

// queryRow runs a statement and materialises at most one row.
//
// The row is copied out and the driver's Rows closed before returning, which
// solves two problems at once. MySQL will not accept another command on this
// connection until the result set is drained, and the driver's []byte values
// point into that connection's read buffer — so anything retained past this
// point would be overwritten the next time the connection is used by someone
// else. Copying here is what makes it safe to release the connection.
func (c *Conn) queryRow(ctx context.Context, query string, args []any) ([]driver.Value, error) {
	stmt, err := c.prepare(ctx, query)
	if err != nil {
		return nil, err
	}

	querier, ok := stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("db: driver statement does not support QueryContext")
	}

	named, err := toNamedValues(args)
	if err != nil {
		return nil, err
	}

	rows, err := querier.QueryContext(ctx, named)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	vals := make([]driver.Value, len(rows.Columns()))
	if err := rows.Next(vals); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, ErrNoRows
		}
		return nil, err
	}

	return cloneValues(vals), nil
}

// ping checks the connection is still alive.
func (c *Conn) ping(ctx context.Context) error {
	pinger, ok := c.raw.(driver.Pinger)
	if !ok {
		return nil // nothing to check; assume healthy
	}
	return pinger.Ping(ctx)
}

// destroy closes the cached statements and the underlying connection. Called
// on eviction and shutdown — never on Release, or the statement cache would be
// pointless.
func (c *Conn) destroy() error {
	for _, stmt := range c.stmtCache {
		_ = stmt.Close()
	}
	c.stmtCache = nil
	return c.raw.Close()
}

// expired reports whether the connection has outlived either limit.
func (c *Conn) expired(now time.Time, maxLifetime, maxIdleTime time.Duration) bool {
	return now.Sub(c.created) > maxLifetime || now.Sub(c.lastUsed) > maxIdleTime
}

// --- the database/sql replacement -----------------------------------------

// Row is the single-row result of a query, already detached from the
// connection. It mirrors *sql.Row closely enough that users.go reads the same.
type Row struct {
	vals []driver.Value
	err  error
}

// Scan copies the row's columns into dest, which must be pointers.
//
// This is the hop database/sql's unexported convertAssign used to do. The
// driver has already turned wire bytes into Go scalars; all that is left is
// getting them into the specific field types the caller asked for.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.vals) {
		return fmt.Errorf("db: Scan got %d destinations, row has %d columns", len(dest), len(r.vals))
	}
	for i, d := range dest {
		if err := scanValue(r.vals[i], d); err != nil {
			return fmt.Errorf("db: scanning column %d: %w", i, err)
		}
	}
	return nil
}

// scanValue assigns one driver.Value to one typed destination.
//
// NULL arrives as nil and becomes the destination's zero value. That is exactly
// what the sql.NullString dance in the previous implementation amounted to for
// avatar_suffix, so collapsing it here removes a step rather than changing
// behaviour. A column where NULL and "" must be told apart would need a
// nullable destination type instead.
func scanValue(src driver.Value, dest any) error {
	switch d := dest.(type) {
	case *string:
		switch s := src.(type) {
		case nil:
			*d = ""
		case string:
			*d = s
		case []byte:
			*d = string(s) // conversion copies, which is required: see cloneValues
		default:
			return fmt.Errorf("cannot assign %T to *string", src)
		}

	case *[]byte:
		switch s := src.(type) {
		case nil:
			*d = nil
		case []byte:
			*d = s // already cloned by cloneValues
		case string:
			*d = []byte(s)
		default:
			return fmt.Errorf("cannot assign %T to *[]byte", src)
		}

	case *uint64:
		v, err := toUint64(src)
		if err != nil {
			return err
		}
		*d = v

	case *uint32:
		v, err := toUint64(src)
		if err != nil {
			return err
		}
		if v > math.MaxUint32 {
			return fmt.Errorf("value %d overflows uint32", v)
		}
		*d = uint32(v)

	case *int64:
		switch s := src.(type) {
		case nil:
			*d = 0
		case int64:
			*d = s
		case uint64:
			if s > math.MaxInt64 {
				return fmt.Errorf("value %d overflows int64", s)
			}
			*d = int64(s)
		default:
			return fmt.Errorf("cannot assign %T to *int64", src)
		}

	case *time.Time:
		switch s := src.(type) {
		case nil:
			*d = time.Time{}
		case time.Time:
			*d = s
		default:
			return fmt.Errorf("cannot assign %T to *time.Time", src)
		}

	default:
		return fmt.Errorf("unsupported scan destination %T", dest)
	}
	return nil
}

// toUint64 normalises the several integer shapes a driver may return. Whether
// an unsigned column arrives as int64 or uint64 depends on the driver's binary
// row decoder, so both are accepted rather than assumed.
func toUint64(src driver.Value) (uint64, error) {
	switch s := src.(type) {
	case nil:
		return 0, nil
	case uint64:
		return s, nil
	case int64:
		if s < 0 {
			return 0, fmt.Errorf("negative value %d in unsigned column", s)
		}
		return uint64(s), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to uint64", src)
	}
}

// cloneValues copies any []byte out of the driver's read buffer.
//
// This is the subtle one. The driver's byte slices alias that connection's
// buffer, whose lifetime is the CONNECTION's, not the row's. Return the
// connection to the pool while still holding those slices and the next
// goroutine to use it will overwrite them — one user's data appearing in
// another's response. database/sql hid this by copying inside Scan.
func cloneValues(vals []driver.Value) []driver.Value {
	out := make([]driver.Value, len(vals))
	for i, v := range vals {
		if b, ok := v.([]byte); ok {
			cp := make([]byte, len(b))
			copy(cp, b)
			out[i] = cp
			continue
		}
		out[i] = v
	}
	return out
}

// toNamedValues converts query arguments into what the driver accepts.
//
// driver.Value is restricted to a handful of types, so uint64 ids have to be
// narrowed to int64 on the way in even though they may come back as uint64.
func toNamedValues(args []any) ([]driver.NamedValue, error) {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		var v driver.Value
		switch t := a.(type) {
		case nil, string, []byte, bool, int64, float64, time.Time:
			v = t
		case int:
			v = int64(t)
		case uint32:
			v = int64(t)
		case uint64:
			if t > math.MaxInt64 {
				return nil, fmt.Errorf("db: argument %d (%d) overflows int64", i, t)
			}
			v = int64(t)
		default:
			return nil, fmt.Errorf("db: unsupported argument type %T at index %d", a, i)
		}
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out, nil
}
