// TODO: Tests for the custom MySQL pool.
//
// Coverage to add:
//   - Concurrent Acquire from many goroutines respects max_open
//   - Acquire blocks when pool exhausted, returns on Release
//   - Acquire returns error if wait timeout exceeded
//   - Health check evicts closed/broken connections
//   - Shutdown drains in-flight and rejects new Acquires
//   - Prepared statement cache: same SQL string returns same handle per connection
//   - Prepared statement cache: different connections have independent caches
//
// May require a real MySQL instance (docker) or a mock connector.
package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entry_profile_creation/internal/config"
)

// testPool builds a pool with explicit sizing. Shares the skip behaviour and
// DSN of users_test.go — see the setup comment there.
func testPool(t *testing.T, maxOpen int, maxWait time.Duration) *Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping MySQL integration test in -short mode")
	}

	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}

	p, err := New(&config.MySQLPoolConfig{MaxOpen: maxOpen, MaxWait: maxWait}, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.PingContext(context.Background()); err != nil {
		p.Close()
		t.Skipf("no MySQL test database reachable (%v)", err)
	}
	t.Cleanup(func() { p.Close() })

	return p
}

// TestPoolRespectsMaxOpen is the core safety property: however many goroutines
// pile in, the number holding a connection at once never exceeds the limit.
// Exceeding it would eventually exhaust MySQL's own max_connections.
func TestPoolRespectsMaxOpen(t *testing.T) {
	const maxOpen = 3
	p := testPool(t, maxOpen, time.Second)
	ctx := context.Background()

	var concurrent, peak atomic.Int64
	var wg sync.WaitGroup

	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			c, err := p.Acquire(ctx)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer p.Release(c)

			n := concurrent.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			// Hold the connection long enough for overlap to be possible.
			if _, err := c.queryRow(ctx, "SELECT 1", nil); err != nil {
				t.Errorf("queryRow: %v", err)
			}
			concurrent.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > maxOpen {
		t.Errorf("peak concurrent checkouts = %d, want <= %d", got, maxOpen)
	}
	if got := p.Stats().InUse; got != 0 {
		t.Errorf("InUse after all releases = %d, want 0; a slot leaked", got)
	}
}

// TestAcquireBlocksThenSucceedsOnRelease checks that a waiter is genuinely
// parked rather than spinning or failing, and is woken by a Release.
func TestAcquireBlocksThenSucceedsOnRelease(t *testing.T) {
	p := testPool(t, 1, 5*time.Second)
	ctx := context.Background()

	held, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		c, err := p.Acquire(ctx)
		if err == nil {
			p.Release(c)
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("second Acquire returned (%v) while the only connection was held", err)
	case <-time.After(150 * time.Millisecond):
		// Still blocked, as it should be.
	}

	p.Release(held)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Acquire after Release = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not wake after Release")
	}
}

// TestAcquireTimesOut covers the load-shedding path: under sustained overload
// callers get a fast error instead of an unbounded queue.
func TestAcquireTimesOut(t *testing.T) {
	p := testPool(t, 1, 50*time.Millisecond)
	ctx := context.Background()

	held, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer p.Release(held)

	before := p.Stats().WaitCount

	start := time.Now()
	if _, err := p.Acquire(ctx); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Acquire = %v, want ErrPoolExhausted", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Acquire took %v to time out, want ~50ms", elapsed)
	}

	// The wait must be visible in stats, since WaitCount climbing is the
	// signal that the pool is undersized.
	if after := p.Stats().WaitCount; after <= before {
		t.Errorf("WaitCount = %d, want > %d", after, before)
	}
}

// TestAcquireRespectsContext distinguishes "the client gave up" from "the pool
// is overloaded" — they are different errors and, upstream, different statuses.
func TestAcquireRespectsContext(t *testing.T) {
	p := testPool(t, 1, 10*time.Second) // wait timeout far beyond the context

	held, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer p.Release(held)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := p.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire = %v, want context.DeadlineExceeded", err)
	}
}

// TestBrokenConnectionIsDiscarded checks that a connection the driver declared
// unusable is destroyed on Release rather than handed to the next caller.
func TestBrokenConnectionIsDiscarded(t *testing.T) {
	p := testPool(t, 2, time.Second)
	ctx := context.Background()

	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	c.broken = true

	before := p.Stats().Evictions
	p.Release(c)

	if got := p.Stats().Idle; got != 0 {
		t.Errorf("Idle = %d, want 0; a broken connection went back into the pool", got)
	}
	if after := p.Stats().Evictions; after != before+1 {
		t.Errorf("Evictions = %d, want %d", after, before+1)
	}

	// The pool must still be usable — it just dials a replacement.
	next, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after discard: %v", err)
	}
	if next == c {
		t.Error("the broken connection was handed out again")
	}
	p.Release(next)
}

// TestEvictStaleRetiresExpiredIdleConnections covers the sweeper. MySQL's
// wait_timeout closes idle sessions server-side, so a pool that never retires
// idle connections accumulates dead sockets.
func TestEvictStaleRetiresExpiredIdleConnections(t *testing.T) {
	p := testPool(t, 2, time.Second)
	ctx := context.Background()

	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p.Release(c)

	if got := p.Stats().Idle; got != 1 {
		t.Fatalf("Idle = %d, want 1 before eviction", got)
	}

	// Backdate the connection past its lifetime instead of waiting minutes.
	p.mu.Lock()
	p.idle[0].created = time.Now().Add(-2 * p.connMaxLifetime)
	p.mu.Unlock()

	p.evictStale()

	if got := p.Stats().Idle; got != 0 {
		t.Errorf("Idle = %d, want 0 after eviction", got)
	}
}

// TestCloseDrainsInFlightAndRejectsNew covers graceful shutdown: Close waits
// for work already checked out, then refuses everything after.
func TestCloseDrainsInFlightAndRejectsNew(t *testing.T) {
	p := testPool(t, 2, time.Second)
	ctx := context.Background()

	held, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()

	select {
	case <-closed:
		t.Fatal("Close returned while a connection was still checked out")
	case <-time.After(150 * time.Millisecond):
		// Still draining, as it should be.
	}

	p.Release(held)

	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after the last Release")
	}

	if _, err := p.Acquire(ctx); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Acquire after Close = %v, want ErrPoolClosed", err)
	}
	// Close must be safe to call twice; main defers it and tests do too.
	if err := p.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestStmtCacheReusesHandle is the point of the per-connection cache: the same
// SQL prepared once, then reused for the life of the connection.
func TestStmtCacheReusesHandle(t *testing.T) {
	p := testPool(t, 1, time.Second)
	ctx := context.Background()

	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer p.Release(c)

	const query = "SELECT 1"
	for range 3 {
		if _, err := c.queryRow(ctx, query, nil); err != nil {
			t.Fatalf("queryRow: %v", err)
		}
	}

	if got := len(c.stmtCache); got != 1 {
		t.Errorf("stmtCache has %d entries after 3 identical queries, want 1", got)
	}

	first := c.stmtCache[query]
	stmt, err := c.prepare(ctx, query)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if stmt != first {
		t.Error("prepare returned a new handle for SQL already in the cache")
	}

	// A different statement gets its own entry.
	if _, err := c.queryRow(ctx, "SELECT 2", nil); err != nil {
		t.Fatalf("queryRow: %v", err)
	}
	if got := len(c.stmtCache); got != 2 {
		t.Errorf("stmtCache has %d entries, want 2", got)
	}
}

// TestStmtCachesAreIndependentPerConnection is the reason the cache lives on
// Conn and not on Pool. A MySQL statement handle is scoped to its session, so
// sharing one across connections would be meaningless at best.
func TestStmtCachesAreIndependentPerConnection(t *testing.T) {
	p := testPool(t, 2, time.Second)
	ctx := context.Background()

	first, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	second, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer p.Release(first)
	defer p.Release(second)

	if first == second {
		t.Fatal("the pool handed out the same connection twice at once")
	}

	const query = "SELECT 1"
	if _, err := first.queryRow(ctx, query, nil); err != nil {
		t.Fatalf("queryRow on first: %v", err)
	}

	if len(second.stmtCache) != 0 {
		t.Errorf("second connection's cache has %d entries, want 0; the caches are shared",
			len(second.stmtCache))
	}

	if _, err := second.queryRow(ctx, query, nil); err != nil {
		t.Fatalf("queryRow on second: %v", err)
	}

	var a, b driver.Stmt = first.stmtCache[query], second.stmtCache[query]
	if a == nil || b == nil {
		t.Fatal("both connections should have cached the statement")
	}
	if a == b {
		t.Error("both connections share one statement handle; it is only valid on one session")
	}
}
