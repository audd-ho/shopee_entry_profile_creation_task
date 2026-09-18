package db

// Minimal placeholder tests. Full integration tests would require a running MySQL
// (docker-compose) and are deferred; add during implementation phase.

import (
	"testing"
	"time"
)

func TestStatsZeroValue(t *testing.T) {
	p := &Pool{maxOpen: 10, maxWait: 100 * time.Millisecond}
	s := p.Stats()
	if s.MaxOpen != 10 || s.OpenNow != 0 || s.Idle != 0 {
		t.Errorf("unexpected stats: %+v", s)
	}
}
