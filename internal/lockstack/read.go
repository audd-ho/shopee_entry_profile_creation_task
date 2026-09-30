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
//     A lease value (L:<token>) is treated as a miss.
//  2. Cache miss — readCheckScript (atomic cache + lock + lease check):
//     a. HIT: cache populated while we waited → serve it
//     b. LOCKED: writer in progress → poll with capped exponential backoff
//     c. LEASED: another reader holds the lease → poll
//     d. GRANTED: we hold the lease → fresh DB read → leaseSet
//  3. On max wait exceeded: DB read, return without caching
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

	// Step 1 (M8): plain GET — if real data, serve immediately.
	raw, err := ls.rdb.Get(ctx, ck).Result()
	if err == nil && !isLeaseValue(raw) {
		if data, version, ok := decodeValue(raw); ok {
			return data, version, nil
		}
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		log.Printf("lockstack: cache GET failed for %s, falling through to DB: %v", ck, err)
		return dbRead(ctx)
	}

	// Step 2: miss path — readCheck loop with lease grant + capped
	// exponential backoff (0.5ms → 1 → 2 → 4ms cap, + jitter).
	token := newLeaseToken()
	start := time.Now()
	backoff := ls.cfg.PollMin

	for {
		res, err := ls.readCheck(ctx, ck, lk, token)
		if err != nil {
			log.Printf("lockstack: readCheck failed for %s: %v", ck, err)
			return dbRead(ctx)
		}

		switch res.state {
		case "HIT":
			if data, version, ok := decodeValue(res.value); ok {
				return data, version, nil
			}
			return dbRead(ctx)

		case "GRANTED":
			return ls.fetchAndFill(ctx, ck, token, dbRead)

		case "LOCKED", "LEASED":
			maxWait := ls.cfg.LeaseTTL
			if res.state == "LOCKED" {
				maxWait = 2 * ls.cfg.LockTTL
			}
			if time.Since(start) > maxWait {
				return dbRead(ctx)
			}
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(backoff + jitter(backoff/4)):
			}
			backoff = min(backoff*2, ls.cfg.PollMax)

		default:
			return dbRead(ctx)
		}
	}
}

// readCheckResult holds the parsed response from readCheckCmd.
type readCheckResult struct {
	state string // "HIT", "LOCKED", "LEASED", or "GRANTED"
	value string // cached value (only set when state == "HIT")
}

// readCheck runs the readCheckCmd Lua script and parses the array result.
func (ls *LockStack) readCheck(ctx context.Context, cacheKey, lockKey, token string) (readCheckResult, error) {
	result, err := readCheckCmd.Run(ctx, ls.rdb, []string{cacheKey, lockKey}, token, ls.cfg.LeaseTTL.Milliseconds()).Slice()
	if err != nil {
		return readCheckResult{}, err
	}
	if len(result) == 0 {
		return readCheckResult{}, errors.New("lockstack: empty readCheck result")
	}
	state, _ := result[0].(string)
	res := readCheckResult{state: state}
	if len(result) > 1 {
		res.value, _ = result[1].(string)
	}
	return res, nil
}

// fetchAndFill reads DB under a granted lease, then populates cache via
// leaseSetCmd. If the lease was voided (writer DEL'd it), the populate is
// silently dropped — data is still returned to the caller.
func (ls *LockStack) fetchAndFill(
	ctx context.Context,
	cacheKey string,
	token string,
	dbRead func(ctx context.Context) ([]byte, int64, error),
) ([]byte, int64, error) {
	// Release the lease on any exit path. Safe to call after a successful
	// leaseSet — the slot no longer holds our lease, so it's a no-op.
	defer ls.releaseLease(cacheKey, token)

	data, version, err := dbRead(ctx)
	if err != nil {
		return nil, 0, err
	}

	encoded := encodeValue(version, data)
	ttl := cacheTTLSeconds(ls.cfg.CacheTTL)
	if setErr := leaseSetCmd.Run(ctx, ls.rdb, []string{cacheKey}, token, encoded, ttl).Err(); setErr != nil {
		log.Printf("lockstack: leaseSet failed for %s: %v", cacheKey, setErr)
	}

	return data, version, nil
}

// releaseLease deletes the lease only if it's still ours. Uses background
// context because this runs in a defer after the request context may have
// been cancelled.
func (ls *LockStack) releaseLease(cacheKey, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = releaseLeaseCmd.Run(ctx, ls.rdb, []string{cacheKey}, token).Err()
}
