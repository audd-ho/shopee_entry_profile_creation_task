package lockstack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Write executes the full lock-stack write path (M2c — the lock is a writer
// lease in the cache slot itself):
//
//  1. Acquire: put LW:<writer_id> into the slot (one Lua call). This is the
//     lock, invalidates the slot (M7) and voids any reader lease; the old
//     data comes back for restoration
//  2. Start watchdog (M3); the lease never outlives the write's deadline
//  3. Call dbWrite under that deadline — only if at least MinWriteBudget is
//     left; a deadlock victim is retried up to MaxDeadlockRetries times,
//     each retry gated the same way
//  4. Finish: write-through with version compare (M5 + M9); replacing the
//     writer lease with data is also the unlock
//
// The deadline is min(caller's deadline, WatchdogTTL) — normally the TCP
// server's per-request timeout.
//
// dbWrite must return the serialized data to cache and its version number.
//
// On a definite DB failure (or if no attempt could start) the old cache value
// is restored. On an unknown outcome — the deadline fired, the connection
// broke mid-statement, or the callback wrapped its error with Committed — the
// slot is left empty, since the DB may hold the new value; the next reader
// leases and reads the DB.
//
// On Redis failure during acquisition, degrades to a lockless DB write +
// best-effort cache delete.
func (ls *LockStack) Write(
	ctx context.Context,
	key string,
	dbWrite func(ctx context.Context) (data []byte, version int64, err error),
) error {
	if ls.cfg.Disabled {
		_, _, err := dbWrite(ctx)
		return err
	}

	ck := cacheKeyFor(key)
	lease := writerLeasePrefix + generateWriterID()

	// One deadline for the whole write — the earlier of the caller's (the
	// request timeout) and WatchdogTTL. The lease is clipped to it and the DB
	// call is cancelled at it, so a writer hung on the DB can't wake up later
	// and write without the lock. MinWriteBudget (above the MySQL lock-wait
	// timeout) makes sure a statement queued in MySQL fails before it.
	ctx, cancel := context.WithTimeout(ctx, ls.cfg.WatchdogTTL)
	defer cancel()

	// Step 1: acquire the writer lease (lock + invalidate in one call).
	savedValue, err := ls.acquireLock(ctx, ck, lease)
	if err != nil {
		// Only a Redis failure degrades to the lockless write — never lock
		// contention or running out of time.
		if errors.Is(err, ErrLockHeld) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return err
		}
		return ls.degradedWrite(ctx, ck, dbWrite)
	}

	// Step 2: start watchdog — extends the lease while the write is in progress.
	watchdogCancel := ls.startWatchdog(ctx, ck, lease)

	// Cleanup on ANY exit path: stop the watchdog and release the lease if
	// it's still ours (empty slot). A no-op after a successful finish.
	// Deferred, so the watchdog keeps the lease alive until the finish (or
	// restore) has replaced it — no gap where the lease could lapse first.
	// Its extend only PEXPIREs while the slot still holds our lease, so it
	// can never touch the data that replaced it.
	defer func() {
		watchdogCancel()
		ls.releaseLock(ck, lease, nil)
	}()

	// Step 3: DB write — the caller's mutation.
	data, version, err := ls.dbWriteWithRetry(ctx, ck, dbWrite)
	if err != nil {
		if isUnknownOutcome(err) {
			// The DB may have committed: restoring the old value could cache
			// data the DB no longer has. Leave the slot empty (deferred release).
			log.Printf("lockstack: write outcome unknown for %s, leaving cache empty: %v", ck, err)
			return err
		}
		// Step 3b: DB definitely failed (or no attempt started) — restore the
		// cache to its pre-write state.
		if len(savedValue) > 0 {
			ls.releaseLock(ck, lease, savedValue)
		}
		return err
	}

	// Step 4: write-through with version compare (M5 + M9), which also
	// unlocks. Populates cache BEFORE readers unblock, so they get a cache
	// hit instead of a DB stampede. Runs on its own context: the DB write
	// committed, and this is what repairs the cache even when the deadline
	// has just passed.
	encoded := encodeValue(version, data)
	if err := ls.writerFinish(ck, lease, version, encoded); err != nil {
		log.Printf("lockstack: write-through failed for %s: %v", ck, err)
	}

	return nil
}

// dbWriteWithRetry runs the caller's DB write, starting each attempt only
// if at least MinWriteBudget is left before the deadline, and retrying only
// deadlock victims (MySQL rolled the statement back, nothing committed).
func (ls *LockStack) dbWriteWithRetry(
	ctx context.Context,
	cacheKey string,
	dbWrite func(ctx context.Context) ([]byte, int64, error),
) ([]byte, int64, error) {
	for attempt := 0; ; attempt++ {
		if left := timeLeft(ctx); left < ls.cfg.MinWriteBudget {
			log.Printf("lockstack: %v left for %s, below MinWriteBudget %v — not starting the DB write", left, cacheKey, ls.cfg.MinWriteBudget)
			return nil, 0, ErrWriteBudget
		}
		data, version, err := dbWrite(ctx)
		if err == nil || isUnknownOutcome(err) || !isDeadlock(err) || attempt >= ls.cfg.MaxDeadlockRetries {
			return data, version, err
		}
		log.Printf("lockstack: deadlock on %s, retrying (attempt %d): %v", cacheKey, attempt+1, err)
		select {
		case <-ctx.Done():
			return nil, 0, err
		case <-time.After(time.Millisecond + jitter(5*time.Millisecond)):
		}
	}
}

// acquireLock places the writer lease, retrying with backoff while another
// writer holds the slot, up to MaxLockRetries. Returns the slot's old value
// when it was real data. The lease TTL is clipped to the deadline, so even
// the initial lease can't outlive the write.
func (ls *LockStack) acquireLock(ctx context.Context, cacheKey, lease string) ([]byte, error) {
	backoff := ls.cfg.LockTTL / time.Duration(ls.cfg.MaxLockRetries)
	if backoff < time.Millisecond {
		backoff = time.Millisecond
	}
	for i := 0; i <= ls.cfg.MaxLockRetries; i++ {
		ttl := min(ls.cfg.LockTTL, timeLeft(ctx))
		if ttl < time.Millisecond {
			return nil, context.DeadlineExceeded
		}
		res, err := writerAcquireCmd.Run(ctx, ls.rdb, []string{cacheKey}, lease, ttl.Milliseconds()).Slice()
		if err != nil {
			return nil, fmt.Errorf("lockstack: redis error: %w", err)
		}
		if len(res) == 0 {
			return nil, errors.New("lockstack: empty acquire result")
		}
		if state, _ := res[0].(string); state == "OK" {
			var saved []byte
			if len(res) > 1 {
				old, _ := res[1].(string)
				saved = []byte(old)
			}
			return saved, nil
		}

		if i == ls.cfg.MaxLockRetries {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, ErrLockHeld
}

// releaseLock releases the writer lease only if we still own it: restores
// savedValue when given, otherwise leaves the slot empty. Uses a background
// context because this runs in a defer after the write's context may have
// expired.
func (ls *LockStack) releaseLock(cacheKey, lease string, savedValue []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), ls.cfg.CleanupTimeout)
	defer cancel()
	ttl := cacheTTLSeconds(ls.cfg.CacheTTL)
	if err := writerReleaseCmd.Run(ctx, ls.rdb, []string{cacheKey}, lease, string(savedValue), ttl).Err(); err != nil {
		log.Printf("lockstack: release lock %s: %v (will expire via TTL)", cacheKey, err)
	}
}

// writerFinish runs the write-through Lua script (version-compare SET that
// also replaces our writer lease). Uses a background context: see Write.
func (ls *LockStack) writerFinish(cacheKey, lease string, version int64, encoded string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ls.cfg.CleanupTimeout)
	defer cancel()
	ttl := cacheTTLSeconds(ls.cfg.CacheTTL)
	return writerFinishCmd.Run(ctx, ls.rdb, []string{cacheKey}, lease, version, encoded, ttl).Err()
}

// degradedWrite skips the lock and runs the DB write directly, then does a
// best-effort cache delete. This is the "Simple" fallback when Redis is down.
func (ls *LockStack) degradedWrite(
	ctx context.Context,
	cacheKey string,
	dbWrite func(ctx context.Context) ([]byte, int64, error),
) error {
	log.Printf("lockstack: Redis unavailable, degrading to lockless write")
	_, _, err := dbWrite(ctx)
	if err != nil {
		return err
	}
	_ = ls.rdb.Del(ctx, cacheKey).Err()
	return nil
}

// timeLeft is the time until ctx's deadline, or effectively unlimited when
// it has none.
func timeLeft(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return time.Duration(1<<63 - 1)
}

// mysqlDeadlock is MySQL's ER_LOCK_DEADLOCK: the statement was chosen as the
// deadlock victim and rolled back, so retrying is safe.
const mysqlDeadlock = 1213

func isDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlDeadlock
}

// isUnknownOutcome reports whether a DB write error leaves it unknown whether
// the mutation committed: the deadline fired or the context was cancelled,
// the connection broke mid-statement, or the callback marked the error as
// happening after its commit (Committed).
func isUnknownOutcome(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, mysql.ErrInvalidConn) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, errCommitted)
}
