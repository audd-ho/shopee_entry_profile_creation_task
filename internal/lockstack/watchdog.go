package lockstack

import (
	"context"
	"log"
	"time"
)

// startWatchdog spawns a goroutine that extends the lock TTL every TTL/3
// while the writer is alive. Returns a cancel function that MUST be called
// when the write completes (in a defer alongside lock release).
//
// If the process crashes, the goroutine dies with it and the lock expires
// naturally after the TTL — safe recovery without any explicit cleanup.
func (ls *LockStack) startWatchdog(ctx context.Context, lockKey, writerID string) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	interval := ls.cfg.LockTTL / 3

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := ls.extendLock(ctx, lockKey, writerID)
				if err != nil || !ok {
					if err != nil && ctx.Err() == nil {
						log.Printf("lockstack: watchdog extend failed for %s: %v", lockKey, err)
					}
					return
				}
				log.Printf("lockstack: watchdog extended lock %s (slow write?)", lockKey)
			}
		}
	}()

	return cancel
}

func (ls *LockStack) extendLock(ctx context.Context, lockKey, writerID string) (bool, error) {
	ttlMs := ls.cfg.LockTTL.Milliseconds()
	result, err := lockExtendCmd.Run(ctx, ls.rdb, []string{lockKey}, writerID, ttlMs).Int64()
	if err != nil {
		return false, err
	}
	return result == 1, nil
}
