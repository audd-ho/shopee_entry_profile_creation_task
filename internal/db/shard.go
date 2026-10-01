// TODO: Shard-aware DB handle (see sharding_design.md §8, sharding_implementation.md §3).
//
// ShardedHandle wraps multiple pools (data shards + lookup shards) and a
// consistent hash ring. Handlers call DataShardFor/LookupShardFor to pick
// the right pool, then pass it to the existing query helpers — the helpers
// themselves are unchanged and shard-unaware.
package db

import (
	"entry_profile_creation/internal/sharding"
)

type shardSet struct {
	pools []*Pool
	ring  *sharding.Ring
}

type ShardedHandle struct {
	data   shardSet // keyed by user_id
	lookup shardSet // keyed by username
}

func NewShardedHandle(dataPools, lookupPools []*Pool, vnodes int) *ShardedHandle {
	return &ShardedHandle{
		data:   shardSet{dataPools, sharding.NewRing(len(dataPools), vnodes)},
		lookup: shardSet{lookupPools, sharding.NewRing(len(lookupPools), vnodes)},
	}
}

// DataShardFor returns the data-shard pool that owns the given userID.
func (sh *ShardedHandle) DataShardFor(userID uint64) *Pool {
	return sh.data.pools[sh.data.ring.ShardFor(userID)]
}

// LookupShardFor returns the lookup-shard pool for the given username.
// Uses FNV-1a hash modulo the number of lookup shards.
func (sh *ShardedHandle) LookupShardFor(username string) *Pool {
	return sh.lookup.pools[sh.lookup.ring.ShardForString(username)]
}

// Close shuts down all data and lookup shard pools.
func (sh *ShardedHandle) Close() error {
	var firstErr error
	for _, p := range sh.data.pools {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, p := range sh.lookup.pools {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// DataShards returns the underlying data shard pools (for the seeder and
// migration tooling that need to iterate all shards).
func (sh *ShardedHandle) DataShards() []*Pool { return sh.data.pools }

// LookupShards returns the underlying lookup shard pools.
func (sh *ShardedHandle) LookupShards() []*Pool { return sh.lookup.pools }

func (sh *ShardedHandle) DataRing() *sharding.Ring   { return sh.data.ring }
func (sh *ShardedHandle) LookupRing() *sharding.Ring { return sh.lookup.ring }
