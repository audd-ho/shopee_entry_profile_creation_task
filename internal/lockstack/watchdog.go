package lockstack

import (
	"context"
	"log"
	"time"
)

// startWatchdog spawns a goroutine that extends the writer lease's TTL every TTL/3
// while the writer is alive, never past WatchdogTTL from the start of the
// write. Returns a cancel function that MUST be called when the write
// completes (in a defer alongside lock release).
//
// If the process crashes, the goroutine dies with it and the lock expires
// naturally after the TTL — safe recovery without any explicit cleanup.
//
// If the writer hangs (process alive, stuck on a call), the goroutine would
// keep extending forever — it can't tell slow from stuck. WatchdogTTL caps
// that: each extension is clipped so the lock never outlives the deadline,
// and the last one lands exactly on it. The deadline is the earlier of
// WatchdogTTL from now and ctx's own deadline (the request timeout), so the
// lock never outlives the write.
func (ls *LockStack) startWatchdog(ctx context.Context, cacheKey, lease string) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	interval := ls.cfg.LockTTL / 3
	deadline := time.Now().Add(ls.cfg.WatchdogTTL)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d // the write's own deadline (request timeout) is earlier
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Extend by LockTTL, or only up to the deadline if it's closer.
				ttl := min(ls.cfg.LockTTL, time.Until(deadline))
				if ttl < time.Millisecond {
					return // deadline reached; the lock's current TTL already ends at it
				}
				ok, err := ls.extendLock(ctx, cacheKey, lease, ttl)
				if err != nil || !ok {
					if err != nil && ctx.Err() == nil {
						log.Printf("lockstack: watchdog extend failed for %s: %v", cacheKey, err)
					}
					return
				}
				if ttl < ls.cfg.LockTTL {
					log.Printf("lockstack: watchdog TTL %v reached for %s, lock expires in %v (writer stuck?)", ls.cfg.WatchdogTTL, cacheKey, ttl)
					return
				}
				log.Printf("lockstack: watchdog extended lock %s (slow write?)", cacheKey)
			}
		}
	}()

	return cancel
}

func (ls *LockStack) extendLock(ctx context.Context, cacheKey, lease string, ttl time.Duration) (bool, error) {
	result, err := lockExtendCmd.Run(ctx, ls.rdb, []string{cacheKey}, lease, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}
