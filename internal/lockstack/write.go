package lockstack

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Write executes the full lock-stack write path:
//
//  1. Acquire per-key Redis lock (M2b)
//  2. Start watchdog (M3)
//  3. Save current cache value (for restoration on failure)
//  4. Delete cache (M7 — before DB write)
//  5. Call dbWrite (the actual DB mutation)
//  6. Write-through to cache with version compare (M5 + M9)
//  7. Release lock
//
// dbWrite must return the serialized data to cache and its version number.
// On DB failure, the old cache value is restored and the DB error is returned.
// On Redis failure during lock acquisition, degrades to a lockless DB write +
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
	lk := lockKeyFor(key)
	writerID := generateWriterID()

	// Step 1: acquire lock.
	if err := ls.acquireLock(ctx, lk, writerID); err != nil {
		if isRedisErr(err) {
			return ls.degradedWrite(ctx, ck, dbWrite)
		}
		return err
	}

	// Step 2: start watchdog — extends the lock while the write is in progress.
	watchdogCancel := ls.startWatchdog(ctx, lk, writerID)

	// Cleanup: cancel watchdog + release lock on ANY exit path.
	defer func() {
		watchdogCancel()
		ls.releaseLock(lk, writerID)
	}()

	// Step 3: save current cache value for restoration on DB failure.
	// A lease value (L:<token>) is not worth restoring.
	savedValue, _ := ls.rdb.Get(ctx, ck).Bytes()
	if isLeaseValue(string(savedValue)) {
		savedValue = nil
	}

	// Step 4: delete cache BEFORE DB write (M7 delete-first ordering).
	_ = ls.rdb.Del(ctx, ck).Err()

	// Step 5: DB write — the caller's mutation.
	data, version, err := dbWrite(ctx)
	if err != nil {
		// Step 5b: DB failed — restore cache to its pre-write state.
		if len(savedValue) > 0 {
			if restoreErr := ls.rdb.Set(ctx, ck, savedValue, ls.cfg.CacheTTL).Err(); restoreErr != nil {
				log.Printf("lockstack: cache restore failed for %s: %v", ck, restoreErr)
			}
		}
		return err
	}

	// Step 6: write-through with version compare (M5 + M9).
	// Populate cache BEFORE releasing the lock so readers unblock into a
	// cache hit instead of a DB stampede.
	encoded := encodeValue(version, data)
	if err := ls.versionSet(ctx, ck, version, encoded); err != nil {
		log.Printf("lockstack: write-through failed for %s: %v", ck, err)
	}

	return nil
}

// acquireLock tries SET NX with TTL, retrying with backoff up to MaxLockRetries.
func (ls *LockStack) acquireLock(ctx context.Context, lockKey, writerID string) error {
	backoff := ls.cfg.LockTTL / time.Duration(ls.cfg.MaxLockRetries)
	if backoff < time.Millisecond {
		backoff = time.Millisecond
	}
	for i := 0; i <= ls.cfg.MaxLockRetries; i++ {
		ok, err := ls.rdb.SetNX(ctx, lockKey, writerID, ls.cfg.LockTTL).Result()
		if err != nil {
			return fmt.Errorf("lockstack: redis error: %w", err)
		}
		if ok {
			return nil
		}

		if i == ls.cfg.MaxLockRetries {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return ErrLockHeld
}

// releaseLock deletes the lock only if we still own it. Uses background context
// because this runs in a defer after the request context may have been cancelled.
func (ls *LockStack) releaseLock(lockKey, writerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lockReleaseCmd.Run(ctx, ls.rdb, []string{lockKey}, writerID).Err(); err != nil {
		log.Printf("lockstack: release lock %s: %v (will expire via TTL)", lockKey, err)
	}
}

// versionSet runs the version-compare-and-SET Lua script.
func (ls *LockStack) versionSet(ctx context.Context, cacheKey string, version int64, encoded string) error {
	ttl := cacheTTLSeconds(ls.cfg.CacheTTL)
	return versionSetCmd.Run(ctx, ls.rdb, []string{cacheKey}, version, encoded, ttl).Err()
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

// isRedisErr returns true for connection/timeout errors, false for logical
// Redis responses (e.g. NX returning false).
func isRedisErr(err error) bool {
	if err == nil {
		return false
	}
	return !isNil(err)
}

func isNil(err error) bool {
	return err != nil && err.Error() == redis.Nil.Error()
}
