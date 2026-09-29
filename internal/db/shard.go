// TODO: Shard-aware DB handle (see sharding_design.md §8, sharding_implementation.md §3).
//
// ShardedHandle wraps multiple pools (data shards + lookup shards) and a
// consistent hash ring. Handlers call DataShardFor/LookupShardFor to pick
// the right pool, then pass it to the existing query helpers — the helpers
// themselves are unchanged and shard-unaware.
package db

import (
	"hash/fnv"

	"entry_profile_creation/internal/sharding"
)

// ShardedHandle is the top-level type that replaces the single *Pool in the
// TCP server. It routes queries to the correct shard pool.
type ShardedHandle struct {
	dataShards   []*Pool
	lookupShards []*Pool
	ring         *sharding.Ring
}

// NewShardedHandle builds a shard-aware handle from pre-opened pools.
func NewShardedHandle(dataShards []*Pool, lookupShards []*Pool, ring *sharding.Ring) *ShardedHandle {
	return &ShardedHandle{
		dataShards:   dataShards,
		lookupShards: lookupShards,
		ring:         ring,
	}
}

// DataShardFor returns the data-shard pool that owns the given userID.
func (sh *ShardedHandle) DataShardFor(userID uint64) *Pool {
	return sh.dataShards[sh.ring.ShardFor(userID)]
}

// LookupShardFor returns the lookup-shard pool for the given username.
// Uses FNV-1a hash modulo the number of lookup shards.
func (sh *ShardedHandle) LookupShardFor(username string) *Pool {
	h := fnv.New32a()
	h.Write([]byte(username))
	return sh.lookupShards[int(h.Sum32())%len(sh.lookupShards)]
}

// Close shuts down all data and lookup shard pools.
func (sh *ShardedHandle) Close() error {
	var firstErr error
	for _, p := range sh.dataShards {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, p := range sh.lookupShards {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// DataShards returns the underlying data shard pools (for the seeder and
// migration tooling that need to iterate all shards).
func (sh *ShardedHandle) DataShards() []*Pool { return sh.dataShards }

// LookupShards returns the underlying lookup shard pools.
func (sh *ShardedHandle) LookupShards() []*Pool { return sh.lookupShards }

// Ring returns the consistent hash ring (for migration tooling that needs
// to compare old vs new ring placement).
func (sh *ShardedHandle) Ring() *sharding.Ring { return sh.ring }
