// TODO: Snowflake ID generator (see sharding_design.md §9).
//
// 64-bit, application-generated, time-sorted IDs.
//
//	┌─────────────────────────────┬──────────┬──────────────┐
//	│  41 bits: ms since custom   │ 10 bits: │ 12 bits:     │
//	│  epoch (~69.7 years range)  │ machine  │ sequence     │
//	│                             │ ID       │ (4096/ms)    │
//	└─────────────────────────────┴──────────┴──────────────┘
package idgen

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// customEpoch: 2026-01-01 00:00:00 UTC. Using a recent epoch maximises the
// 41-bit timestamp space (~69.7 years from this date, through ~2095).
var customEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

const (
	machineBits  = 10
	sequenceBits = 12

	maxMachineID = (1 << machineBits) - 1  // 1023
	maxSequence  = (1 << sequenceBits) - 1 // 4095

	machineShift   = sequenceBits                // 12
	timestampShift = machineBits + sequenceBits   // 22
)

var ErrClockBackward = errors.New("idgen: clock moved backward; refusing to generate")

type Generator struct {
	machineID uint64
	lastMs    uint64
	sequence  uint64
	mu        sync.Mutex
}

func New(machineID int) (*Generator, error) {
	if machineID < 0 || machineID > maxMachineID {
		return nil, fmt.Errorf("idgen: machineID %d out of range [0, %d]", machineID, maxMachineID)
	}
	return &Generator{machineID: uint64(machineID)}, nil
}

// Next generates a globally unique, time-sorted 64-bit ID.
//
// Returns ErrClockBackward if the system clock has jumped backward (fail-fast).
// On sequence exhaustion within a single millisecond (>4096 IDs/ms), busy-waits
// until the next millisecond — effectively never happens for user signups.
func (g *Generator) Next() (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := millisSinceEpoch()
	if now < g.lastMs {
		return 0, ErrClockBackward
	}

	if now == g.lastMs {
		g.sequence++
		if g.sequence > maxSequence {
			for now <= g.lastMs {
				now = millisSinceEpoch()
			}
			g.sequence = 0
		}
	} else {
		g.sequence = 0
	}
	g.lastMs = now

	return (now << timestampShift) | (g.machineID << machineShift) | g.sequence, nil
}

func millisSinceEpoch() uint64 {
	return uint64(time.Now().UnixMilli() - customEpoch)
}
