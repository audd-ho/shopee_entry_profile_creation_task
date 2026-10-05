package lockstack

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// miniredis never counts TTLs down on its own, so a TTL that is back at
// LockTTL means the watchdog extended it. The tests shrink the TTL by hand
// and look at whether it comes back.

func newTestLockStack(t *testing.T, cfg Config) (*LockStack, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ls, err := New(rdb, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ls, mr
}

func TestWatchdogStopsAtWatchdogTTL(t *testing.T) {
	// Ticks every 50ms. Extensions are a full 150ms until the deadline is
	// closer than that (after t=350ms); the tick at t=400ms extends by only
	// the ~100ms left and stops, so the lock expires at t=500ms.
	const lockTTL = 150 * time.Millisecond
	ls, mr := newTestLockStack(t, Config{LockTTL: lockTTL, WatchdogTTL: 500 * time.Millisecond})

	lk, owner := cacheKeyFor("user:1"), writerLeasePrefix+"writer-1"
	mr.Set(lk, owner)
	mr.SetTTL(lk, lockTTL)

	// The writer never finishes: the watchdog is only stopped by its own TTL.
	cancel := ls.startWatchdog(context.Background(), lk, owner)
	defer cancel()

	// Well before the deadline: the shrunk TTL is pushed back up to LockTTL.
	time.Sleep(75 * time.Millisecond)
	mr.SetTTL(lk, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if got := mr.TTL(lk); got != lockTTL {
		t.Fatalf("before deadline: lock TTL = %v, want %v (watchdog should extend)", got, lockTTL)
	}

	// After the clipped extension: shorter than LockTTL, ending at the deadline.
	time.Sleep(325 * time.Millisecond)
	if got := mr.TTL(lk); got <= 0 || got >= lockTTL {
		t.Fatalf("near deadline: lock TTL = %v, want a clipped extension in (0, %v)", got, lockTTL)
	}

	// And the watchdog has stopped: a shrunk TTL stays shrunk.
	mr.SetTTL(lk, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if got := mr.TTL(lk); got != time.Millisecond {
		t.Fatalf("past deadline: lock TTL = %v, want %v (watchdog should have stopped)", got, time.Millisecond)
	}
}

func TestWatchdogTTLDefaults(t *testing.T) {
	ls, _ := newTestLockStack(t, Config{})
	if ls.cfg.WatchdogTTL != 4*time.Second {
		t.Errorf("default WatchdogTTL = %v, want 4s", ls.cfg.WatchdogTTL)
	}
	if ls.cfg.MinWriteBudget != 1500*time.Millisecond {
		t.Errorf("default MinWriteBudget = %v, want 1.5s", ls.cfg.MinWriteBudget)
	}
	if ls.cfg.MaxDeadlockRetries != 2 {
		t.Errorf("default MaxDeadlockRetries = %v, want 2", ls.cfg.MaxDeadlockRetries)
	}
	if ls.cfg.MaxLockedWait != 4*time.Second {
		t.Errorf("default MaxLockedWait = %v, want 2 × LockTTL = 4s", ls.cfg.MaxLockedWait)
	}
	if ls.cfg.CleanupTimeout != 2*time.Second {
		t.Errorf("default CleanupTimeout = %v, want 2s", ls.cfg.CleanupTimeout)
	}

	// Below LockTTL it's raised: SET NX already holds the lock for LockTTL.
	ls, _ = newTestLockStack(t, Config{LockTTL: 2 * time.Second, WatchdogTTL: time.Second})
	if ls.cfg.WatchdogTTL != 2*time.Second {
		t.Errorf("WatchdogTTL below LockTTL = %v, want raised to 2s", ls.cfg.WatchdogTTL)
	}
}

func TestWatchdogNeverTouchesAReplacedSlot(t *testing.T) {
	// Once the finish (or a restore) has replaced the writer lease with data,
	// the watchdog's extend must be a no-op: it checks the slot still holds
	// this writer's lease atomically with the PEXPIRE, and never SETs.
	const lockTTL = 60 * time.Millisecond // extends every 20ms
	ls, mr := newTestLockStack(t, Config{LockTTL: lockTTL, WatchdogTTL: time.Second})

	ck, lease := cacheKeyFor("user:1"), writerLeasePrefix+"writer-1"
	data := encodeValue(6, []byte("new"))
	mr.Set(ck, data) // write-through already replaced the lease
	mr.SetTTL(ck, time.Hour)

	cancel := ls.startWatchdog(context.Background(), ck, lease)
	defer cancel()
	time.Sleep(70 * time.Millisecond) // several ticks

	if got, _ := mr.Get(ck); got != data {
		t.Errorf("slot = %q, want the data untouched", got)
	}
	if got := mr.TTL(ck); got != time.Hour {
		t.Errorf("TTL = %v, want the data's CacheTTL untouched (no PEXPIRE)", got)
	}
}

func TestWatchdogClippedToContextDeadline(t *testing.T) {
	// The request's deadline is earlier than WatchdogTTL: extensions must be
	// clipped to it, so the lease can't outlive the request.
	const lockTTL = 150 * time.Millisecond // extends every 50ms
	ls, mr := newTestLockStack(t, Config{LockTTL: lockTTL, WatchdogTTL: 10 * time.Second})

	ck, lease := cacheKeyFor("user:1"), writerLeasePrefix+"writer-1"
	mr.Set(ck, lease)
	mr.SetTTL(ck, lockTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	stop := ls.startWatchdog(ctx, ck, lease)
	defer stop()

	time.Sleep(75 * time.Millisecond) // the tick at 50ms has run, with ~70ms left
	if got := mr.TTL(ck); got <= 0 || got >= lockTTL {
		t.Fatalf("lock TTL = %v, want an extension clipped to the request deadline (< %v)", got, lockTTL)
	}
}
