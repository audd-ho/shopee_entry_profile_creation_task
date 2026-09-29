package lockstack

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Read executes the lock-stack read path:
//
//  1. GET cache — serve immediately on hit (M8: serve-if-exists)
//  2. Cache miss — Lua check: cache + lock atomically
//     a. Cache appeared while waiting → serve it
//     b. Lock held → poll/wait (bounded by MaxReadWait)
//     c. No cache, no lock → fall through to DB
//  3. Call dbRead (DB fallback)
//  4. Version-compare SET to populate cache (M5)
//  5. Return data + version
//
// dbRead must return the serialized data and its version number.
func (ls *LockStack) Read(
	ctx context.Context,
	key string,
	dbRead func(ctx context.Context) (data []byte, version int64, err error),
) ([]byte, int64, error) {
	if ls.cfg.Disabled {
		return dbRead(ctx)
	}

	ck := cacheKeyFor(key)
	lk := lockKeyFor(key)

	// Step 1 (M8): try a plain GET first — if hit, serve immediately.
	raw, err := ls.rdb.Get(ctx, ck).Result()
	if err == nil {
		if data, version, ok := decodeValue(raw); ok {
			return data, version, nil
		}
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("lockstack: cache GET failed for %s, falling through to DB: %v", ck, err)
		return ls.readFromDB(ctx, ck, lk, dbRead)
	}

	// Step 2: cache miss — check lock with Lua (atomic cache re-check + lock check).
	deadline := time.Now().Add(ls.cfg.MaxReadWait)
	for time.Now().Before(deadline) {
		result, err := ls.rdb.EvalSha(ctx, ls.readCheckSHA, []string{ck, lk}).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			log.Printf("lockstack: read-check script failed for %s: %v", ck, err)
			break
		}

		switch v := result.(type) {
		case string:
			if v == lockedSentinel {
				select {
				case <-ctx.Done():
					return nil, 0, ctx.Err()
				case <-time.After(ls.cfg.ReadPollInterval):
				}
				continue
			}
			// Cache was populated while we were waiting — serve it.
			if data, version, ok := decodeValue(v); ok {
				return data, version, nil
			}
		}

		// nil / false / unparseable → no cache, no lock → fall through to DB.
		break
	}

	// Step 3 + 4: DB read, then lock-aware version-compare SET.
	return ls.readFromDB(ctx, ck, lk, dbRead)
}

// readFromDB calls the DB fallback, then populates cache using the
// reader-specific Lua script that checks the lock before writing. If a
// writer is active, the cache write is skipped (the writer's write-through
// will handle it). If the version-compare rejects the write (newer version
// already cached), that's also fine — the data from DB is still correct to
// serve.
func (ls *LockStack) readFromDB(
	ctx context.Context,
	cacheKey string,
	lockKey string,
	dbRead func(ctx context.Context) (data []byte, version int64, err error),
) ([]byte, int64, error) {
	data, version, err := dbRead(ctx)
	if err != nil {
		return nil, 0, err
	}

	encoded := encodeValue(version, data)
	if setErr := ls.readerVersionSet(ctx, cacheKey, lockKey, version, encoded); setErr != nil {
		log.Printf("lockstack: cache populate failed for %s: %v", cacheKey, setErr)
	}

	return data, version, nil
}

// readerVersionSet runs the lock-aware version-compare-and-SET Lua script.
// Returns nil even when the write is skipped (lock held) or rejected (newer
// version exists) — those are normal, not errors.
func (ls *LockStack) readerVersionSet(ctx context.Context, cacheKey, lockKey string, version int64, encoded string) error {
	ttl := cacheTTLSeconds(ls.cfg.CacheTTL)
	_, err := ls.rdb.EvalSha(ctx, ls.readerVersionSetSHA, []string{cacheKey, lockKey}, version, encoded, ttl).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}
