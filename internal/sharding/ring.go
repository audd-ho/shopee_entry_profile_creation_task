// TODO: Consistent hash ring (see sharding_design.md §3, §8).
//
// Sorted slice of (hash, shardIndex) vnodes. For each key, hash it, binary
// search for the first vnode >= hash (wrapping around). O(log V) per lookup.
package sharding

import (
	"encoding/binary"
	"sort"

	"github.com/cespare/xxhash/v2"
)

const defaultVnodesPerShard = 150

type vnode struct {
	hash  uint64
	shard int
}

type Ring struct {
	vnodes []vnode
	count  int
}

// NewRing builds a consistent hash ring with the given number of shards.
// Each shard gets vnodesPerShard positions on the ring for even distribution.
func NewRing(shardCount, vnodesPerShard int) *Ring {
	if vnodesPerShard <= 0 {
		vnodesPerShard = defaultVnodesPerShard
	}

	total := shardCount * vnodesPerShard
	vnodes := make([]vnode, 0, total)

	var buf [12]byte
	for s := range shardCount {
		for v := range vnodesPerShard {
			binary.LittleEndian.PutUint32(buf[0:4], uint32(s))
			binary.LittleEndian.PutUint64(buf[4:12], uint64(v))
			h := xxhash.Sum64(buf[:])
			vnodes = append(vnodes, vnode{hash: h, shard: s})
		}
	}

	sort.Slice(vnodes, func(i, j int) bool {
		return vnodes[i].hash < vnodes[j].hash
	})

	return &Ring{vnodes: vnodes, count: shardCount}
}

// ShardFor returns the shard index that owns the given key.
func (r *Ring) ShardFor(key uint64) int {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], key)
	h := xxhash.Sum64(buf[:])

	idx := sort.Search(len(r.vnodes), func(i int) bool {
		return r.vnodes[i].hash >= h
	})
	if idx == len(r.vnodes) {
		idx = 0
	}
	return r.vnodes[idx].shard
}

// ShardCount returns the number of shards in the ring.
func (r *Ring) ShardCount() int {
	return r.count
}
