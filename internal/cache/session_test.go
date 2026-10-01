package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"entry_profile_creation/internal/config"
)

// These tests run against miniredis, an in-process Redis implementation, rather
// than the real server. Two reasons: the suite stays runnable with nothing
// installed, and miniredis exposes a controllable clock, so TTL behaviour can
// be tested by fast-forwarding hours instead of sleeping.
func newTestClient(t *testing.T, ttl time.Duration) (*Client, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t) // RunT registers its own cleanup
	c, err := New(&config.RedisConfig{Addr: mr.Addr(), PoolSize: 10}, ttl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	return c, mr
}

const testTTL = 24 * time.Hour

// TestSessionSetAndGet is the basic round trip: what goes in comes back out.
func TestSessionSetAndGet(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "203.0.113.7")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("token length = %d, want 64 hex chars", len(token))
	}

	got, err := c.GetSession(ctx, token)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if got.UserID != 42 {
		t.Errorf("UserID = %d, want 42", got.UserID)
	}
	if got.IP != "203.0.113.7" {
		t.Errorf("IP = %q, want %q", got.IP, "203.0.113.7")
	}
	// Token lives in the key, so GetSession has to put it back on the struct.
	if got.Token != token {
		t.Errorf("Token = %q, want it populated from the key", got.Token)
	}
	if got.CreatedAt.IsZero() || got.LastSeen.IsZero() {
		t.Error("CreatedAt/LastSeen should be set at creation")
	}
}

// TestSessionTokensAreDistinct guards against a broken token source: two
// sessions sharing a token would hand one user another user's account.
func TestSessionTokensAreDistinct(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	seen := make(map[string]bool, 100)
	for range 100 {
		token, err := c.CreateSession(ctx, 1, "")
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if seen[token] {
			t.Fatalf("duplicate token generated: %q", token)
		}
		seen[token] = true
	}
}

// TestSessionStoredValueOmitsToken documents the `json:"-"` on Session.Token:
// the token is the lookup key, storing it again in the value would be dead
// weight on every read.
func TestSessionStoredValueOmitsToken(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stored, err := mr.Get(sessionKey(token))
	if err != nil {
		t.Fatalf("reading raw key: %v", err)
	}
	if strings.Contains(stored, token) {
		t.Errorf("stored value contains the token: %s", stored)
	}
	if !strings.Contains(stored, `"user_id":42`) {
		t.Errorf("stored value missing user_id: %s", stored)
	}
}

func TestGetSessionUnknownToken(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	for _, token := range []string{"", "not-a-real-token"} {
		_, err := c.GetSession(ctx, token)
		if !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("GetSession(%q) error = %v, want ErrSessionNotFound", token, err)
		}
	}
}

func TestDeleteSession(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := c.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := c.GetSession(ctx, token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("after delete, error = %v, want ErrSessionNotFound", err)
	}

	// Logout must be safe to repeat, so deleting an absent key is not an error.
	if err := c.DeleteSession(ctx, token); err != nil {
		t.Errorf("second DeleteSession: %v, want nil", err)
	}
}

// TestSessionExpires checks that the TTL alone reclaims abandoned sessions;
// there is no sweeper anywhere in the service.
func TestSessionExpires(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	mr.FastForward(testTTL + time.Second)

	if _, err := c.GetSession(ctx, token); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("after TTL, error = %v, want ErrSessionNotFound", err)
	}
}

// TestSlidingWindowHoldsOffEarly asserts the optimisation, not just the
// feature: a read in the first half of the lifetime must not write, otherwise
// every authenticated request becomes a Redis write.
func TestSlidingWindowHoldsOffEarly(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	mr.FastForward(time.Hour) // 23h left, well inside the first half
	if _, err := c.GetSession(ctx, token); err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if remaining := mr.TTL(sessionKey(token)); remaining != testTTL-time.Hour {
		t.Errorf("TTL = %v, want %v (an early read must not refresh)", remaining, testTTL-time.Hour)
	}
}

// TestSlidingWindowRefreshesLate is the counterpart: past the halfway mark, a
// read extends the session back to a full lifetime.
func TestSlidingWindowRefreshesLate(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	first, err := c.GetSession(ctx, token)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	mr.FastForward(13 * time.Hour) // 11h left, past the halfway mark
	second, err := c.GetSession(ctx, token)
	if err != nil {
		t.Fatalf("GetSession after fast-forward: %v", err)
	}

	if remaining := mr.TTL(sessionKey(token)); remaining != testTTL {
		t.Errorf("TTL = %v, want the full %v after a late read", remaining, testTTL)
	}
	if !second.LastSeen.After(first.LastSeen) {
		t.Errorf("LastSeen = %v, want it advanced past %v", second.LastSeen, first.LastSeen)
	}
	// The refresh must not disturb anything else in the value.
	if second.UserID != first.UserID || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("refresh altered the session: %+v vs %+v", second, first)
	}
}

// TestSlidingWindowSubSecondPrecision is a regression test for a real bug. The
// refresh check first used Redis TTL, which answers in whole seconds: with a 2s
// lifetime and 1.2s elapsed, 800ms remaining is reported as "1s", so
// "remaining < TTL/2" compared 1s < 1s and never fired. PTTL answers in
// milliseconds and makes the comparison mean what it reads as.
//
// The 24h tests above cannot catch this, because at that scale a whole second
// of rounding is invisible.
func TestSlidingWindowSubSecondPrecision(t *testing.T) {
	const shortTTL = 2 * time.Second
	c, mr := newTestClient(t, shortTTL)
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	mr.FastForward(1200 * time.Millisecond) // 800ms left: past halfway
	if _, err := c.GetSession(ctx, token); err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if remaining := mr.TTL(sessionKey(token)); remaining != shortTTL {
		t.Errorf("TTL = %v, want the full %v; sub-second remaining time was "+
			"rounded away and the refresh never fired", remaining, shortTTL)
	}
}
