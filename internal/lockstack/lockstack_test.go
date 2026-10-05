package lockstack

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// The write and read paths run against miniredis, which executes the Lua
// scripts. Races are staged deterministically from inside the DB callbacks:
// whatever the callback does to the slot happens "while the DB call runs".

func testConfig() Config {
	return Config{
		LockTTL:        200 * time.Millisecond,
		WatchdogTTL:    time.Second,
		MinWriteBudget: 100 * time.Millisecond,
		LeaseTTL:       100 * time.Millisecond,
		CacheTTL:       time.Hour,
		MaxLockRetries: 5,
	}
}

func dbWriteOK(data string, version int64) func(context.Context) ([]byte, int64, error) {
	return func(context.Context) ([]byte, int64, error) { return []byte(data), version, nil }
}

func TestWriteHappyPath(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	mr.Set(ck, encodeValue(5, []byte(`{"nick":"old"}`)))

	if err := ls.Write(context.Background(), "user:1", dbWriteOK(`{"nick":"new"}`, 6)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, _ := mr.Get(ck)
	if want := encodeValue(6, []byte(`{"nick":"new"}`)); got != want {
		t.Errorf("slot = %q, want %q (write-through replaces the writer lease)", got, want)
	}
}

func TestWriteHoldsWriterLeaseDuringDBWrite(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	mr.Set(ck, encodeValue(5, []byte("old")))

	var during string
	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		during, _ = mr.Get(ck)
		return []byte("new"), 6, nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Acquiring is the lock AND the delete-first invalidation: old data gone.
	if !strings.HasPrefix(during, writerLeasePrefix) {
		t.Errorf("slot during DB write = %q, want a writer lease", during)
	}
}

func TestWriteOverwritesReaderLeaseOnAcquire(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	mr.Set(ck, readerLeasePrefix+"reader-1") // a reader is mid-fill

	var during string
	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		during, _ = mr.Get(ck)
		return []byte("new"), 6, nil
	})
	if err != nil {
		t.Fatalf("Write: %v (a reader lease must never block a writer)", err)
	}
	if !strings.HasPrefix(during, writerLeasePrefix) {
		t.Errorf("slot during DB write = %q, want the reader lease replaced by a writer lease", during)
	}
}

func TestWriteWaitsOnAnotherWriter(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	mr.Set(ck, writerLeasePrefix+"other")

	called := false
	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		called = true
		return []byte("new"), 6, nil
	})
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("Write = %v, want ErrLockHeld", err)
	}
	if called {
		t.Error("DB write ran without the lock — lock contention must not degrade to a lockless write")
	}
}

func TestWriteRestoresOnDefiniteDBFailure(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	old := encodeValue(5, []byte("old"))
	mr.Set(ck, old)

	dbErr := errors.New("duplicate key")
	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		return nil, 0, dbErr
	})
	if !errors.Is(err, dbErr) {
		t.Fatalf("Write = %v, want the DB error", err)
	}
	if got, _ := mr.Get(ck); got != old {
		t.Errorf("slot = %q, want the old value restored", got)
	}
}

func TestWriteLeavesSlotEmptyOnUnknownOutcome(t *testing.T) {
	for name, dbErr := range map[string]error{
		"deadline":     context.DeadlineExceeded,
		"after commit": Committed(errors.New("read back failed")),
	} {
		t.Run(name, func(t *testing.T) {
			ls, mr := newTestLockStack(t, testConfig())
			ck := cacheKeyFor("user:1")
			mr.Set(ck, encodeValue(5, []byte("old")))

			err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
				return nil, 0, dbErr
			})
			if err == nil {
				t.Fatal("Write succeeded, want the DB error")
			}
			// The DB may hold the new value: restoring the old one would be stale.
			if mr.Exists(ck) {
				got, _ := mr.Get(ck)
				t.Errorf("slot = %q, want empty (unknown outcome must not restore)", got)
			}
		})
	}
}

func TestWriteFinishOverwritesStrayReaderLease(t *testing.T) {
	// The writer's lease expired mid-write and a reader leased the slot before
	// the commit: that reader's DB read is old, so the write-through must void
	// its lease rather than require owning the slot.
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")

	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		mr.Set(ck, readerLeasePrefix+"late-reader")
		return []byte("new"), 6, nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, _ := mr.Get(ck); got != encodeValue(6, []byte("new")) {
		t.Errorf("slot = %q, want the new data over the stray reader lease", got)
	}
}

func TestWriteFinishSkipsAnotherWritersLease(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")

	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		mr.Set(ck, writerLeasePrefix+"next-writer") // our lease expired, another writer took it
		return []byte("new"), 6, nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, _ := mr.Get(ck); got != writerLeasePrefix+"next-writer" {
		t.Errorf("slot = %q, want the other writer's lease untouched", got)
	}
}

func TestWriteFinishRejectsOlderVersion(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	newer := encodeValue(7, []byte("newer"))

	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		mr.Set(ck, newer) // a newer write landed while ours was paused
		return []byte("older"), 6, nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, _ := mr.Get(ck); got != newer {
		t.Errorf("slot = %q, want the newer version kept", got)
	}
}

func dbReadOK(calls *int, data string, version int64) func(context.Context) ([]byte, int64, error) {
	return func(context.Context) ([]byte, int64, error) {
		*calls++
		return []byte(data), version, nil
	}
}

func TestReadHit(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	mr.Set(cacheKeyFor("user:1"), encodeValue(5, []byte("cached")))

	calls := 0
	data, version, err := ls.Read(context.Background(), "user:1", dbReadOK(&calls, "db", 5))
	if err != nil || string(data) != "cached" || version != 5 {
		t.Fatalf("Read = %q, %d, %v; want cached, 5", data, version, err)
	}
	if calls != 0 {
		t.Errorf("DB read %d times on a cache hit", calls)
	}
}

func TestReadMissLeasesAndFills(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")

	calls := 0
	data, _, err := ls.Read(context.Background(), "user:1", func(ctx context.Context) ([]byte, int64, error) {
		if got, _ := mr.Get(ck); !strings.HasPrefix(got, readerLeasePrefix) {
			t.Errorf("slot during DB read = %q, want a reader lease", got)
		}
		return dbReadOK(&calls, "fresh", 5)(ctx)
	})
	if err != nil || string(data) != "fresh" {
		t.Fatalf("Read = %q, %v; want fresh", data, err)
	}
	if got, _ := mr.Get(ck); got != encodeValue(5, []byte("fresh")) {
		t.Errorf("slot = %q, want the DB result cached", got)
	}
}

func TestReadFillDroppedWhenWriterTakesSlot(t *testing.T) {
	// Slow reader: a writer acquires while the reader is reading the DB. The
	// writer lease replaces the reader lease, so the reader's (possibly old)
	// result must not be cached.
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")

	data, _, err := ls.Read(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		mr.Set(ck, writerLeasePrefix+"writer")
		return []byte("old"), 5, nil
	})
	if err != nil || string(data) != "old" {
		t.Fatalf("Read = %q, %v; want the DB result returned", data, err)
	}
	if got, _ := mr.Get(ck); got != writerLeasePrefix+"writer" {
		t.Errorf("slot = %q, want the writer lease untouched (fill dropped)", got)
	}
}

func TestReadWaitsOnWriterThenFallsBackUncached(t *testing.T) {
	cfg := testConfig()
	cfg.LockTTL = 20 * time.Millisecond // LOCKED max wait = 2 × LockTTL
	ls, mr := newTestLockStack(t, cfg)
	ck := cacheKeyFor("user:1")
	mr.Set(ck, writerLeasePrefix+"stuck-writer")

	calls := 0
	data, _, err := ls.Read(context.Background(), "user:1", dbReadOK(&calls, "db", 5))
	if err != nil || string(data) != "db" || calls != 1 {
		t.Fatalf("Read = %q, %v, %d DB calls; want one uncached DB read", data, err, calls)
	}
	if got, _ := mr.Get(ck); got != writerLeasePrefix+"stuck-writer" {
		t.Errorf("slot = %q, want the writer lease untouched", got)
	}
}

func TestReadServesWriteThroughAfterWait(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	mr.Set(ck, writerLeasePrefix+"writer")

	go func() {
		time.Sleep(5 * time.Millisecond)
		mr.Set(ck, encodeValue(6, []byte("written"))) // writer's finish
	}()

	calls := 0
	data, _, err := ls.Read(context.Background(), "user:1", dbReadOK(&calls, "db", 6))
	if err != nil || string(data) != "written" || calls != 0 {
		t.Fatalf("Read = %q, %v, %d DB calls; want the write-through, no DB read", data, err, calls)
	}
}

func deadlockErr() error { return &mysql.MySQLError{Number: 1213, Message: "Deadlock found"} }

func TestWriteRetriesDeadlock(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")

	calls := 0
	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		calls++
		if calls == 1 {
			return nil, 0, deadlockErr()
		}
		return []byte("new"), 6, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("Write = %v after %d calls; want success on the retry", err, calls)
	}
	if got, _ := mr.Get(ck); got != encodeValue(6, []byte("new")) {
		t.Errorf("slot = %q, want the new data", got)
	}
}

func TestWriteDeadlockRetriesExhausted(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig())
	ck := cacheKeyFor("user:1")
	old := encodeValue(5, []byte("old"))
	mr.Set(ck, old)

	calls := 0
	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		calls++
		return nil, 0, deadlockErr()
	})
	if err == nil || calls != 3 { // 1 attempt + MaxDeadlockRetries (2)
		t.Fatalf("Write = %v after %d calls; want an error after 3 attempts", err, calls)
	}
	if got, _ := mr.Get(ck); got != old {
		t.Errorf("slot = %q, want the old value restored (deadlock = nothing committed)", got)
	}
}

func TestWriteDoesNotRetryLockWaitTimeout(t *testing.T) {
	ls, _ := newTestLockStack(t, testConfig())

	calls := 0
	err := ls.Write(context.Background(), "user:1", func(context.Context) ([]byte, int64, error) {
		calls++
		return nil, 0, &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"}
	})
	if err == nil || calls != 1 {
		t.Fatalf("Write = %v after %d calls; want one attempt, no retry", err, calls)
	}
}

func TestWriteRefusesWithoutBudget(t *testing.T) {
	cfg := testConfig()
	cfg.MinWriteBudget = 200 * time.Millisecond
	ls, mr := newTestLockStack(t, cfg)
	ck := cacheKeyFor("user:1")
	old := encodeValue(5, []byte("old"))
	mr.Set(ck, old)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	called := false
	err := ls.Write(ctx, "user:1", func(context.Context) ([]byte, int64, error) {
		called = true
		return []byte("new"), 6, nil
	})
	if !errors.Is(err, ErrWriteBudget) {
		t.Fatalf("Write = %v, want ErrWriteBudget", err)
	}
	if called {
		t.Error("DB write started with less than MinWriteBudget left")
	}
	if got, _ := mr.Get(ck); got != old {
		t.Errorf("slot = %q, want the old value restored (nothing was sent)", got)
	}
}

func TestWriteLeaseClippedToRequestDeadline(t *testing.T) {
	ls, mr := newTestLockStack(t, testConfig()) // LockTTL 200ms
	ck := cacheKeyFor("user:1")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var ttl time.Duration
	err := ls.Write(ctx, "user:1", func(context.Context) ([]byte, int64, error) {
		ttl = mr.TTL(ck)
		return []byte("new"), 6, nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ttl <= 0 || ttl > 150*time.Millisecond {
		t.Errorf("writer lease TTL = %v, want it clipped to the 150ms request deadline", ttl)
	}
}

func TestReadStopsWaitingBeforeRequestDeadline(t *testing.T) {
	cfg := testConfig()
	cfg.LockTTL = 2 * time.Second // LOCKED max wait would be 4s
	ls, mr := newTestLockStack(t, cfg)
	mr.Set(cacheKeyFor("user:1"), writerLeasePrefix+"stuck-writer")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	calls := 0
	start := time.Now()
	data, _, err := ls.Read(ctx, "user:1", dbReadOK(&calls, "db", 5))
	if err != nil || string(data) != "db" || calls != 1 {
		t.Fatalf("Read = %q, %v, %d DB calls; want an uncached DB read before the deadline", data, err, calls)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Errorf("Read took %v, want it to fall back while LeaseTTL was still left", elapsed)
	}
}
