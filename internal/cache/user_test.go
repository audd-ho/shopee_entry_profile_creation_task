package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/model"
)

// The auth and user caches are exercised against miniredis for the same
// reasons the session tests are: no server to install, and a clock that can be
// fast-forwarded to check TTLs without sleeping for an hour.

func sampleAuth() *model.AuthCached {
	return &model.AuthCached{
		ID:           42,
		PasswordHash: []byte("$2a$10$abcdefghijklmnopqrstuv"),
	}
}

func TestAuthCacheRoundTrip(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	if err := c.SetAuthCache(ctx, "alice", sampleAuth()); err != nil {
		t.Fatalf("SetAuthCache: %v", err)
	}

	got, hit, err := c.GetAuthCache(ctx, "alice")
	if err != nil {
		t.Fatalf("GetAuthCache: %v", err)
	}
	if !hit {
		t.Fatal("expected a hit for a key just written")
	}
	// The hash in particular must survive the round trip: it is what lets a
	// cache hit verify a password without touching MySQL.
	if string(got.PasswordHash) != string(sampleAuth().PasswordHash) {
		t.Errorf("password hash = %q, want %q", got.PasswordHash, sampleAuth().PasswordHash)
	}
	if got.ID != 42 {
		t.Errorf("ID = %d, want 42", got.ID)
	}
}

// A miss must be reported as hit=false with a nil error, never as an error.
// Conflating the two would make an uncached user look like a broken Redis, or
// worse, like a user that does not exist.
func TestAuthCacheMissIsNotAnError(t *testing.T) {
	c, _ := newTestClient(t, testTTL)

	got, hit, err := c.GetAuthCache(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("GetAuthCache on a missing key returned an error: %v", err)
	}
	if hit || got != nil {
		t.Errorf("hit = %v, got = %+v; want false, nil", hit, got)
	}
}

// A malformed entry is treated as a miss so the caller falls through to MySQL
// and overwrites it, rather than one bad key breaking a user until it expires.
func TestAuthCacheCorruptEntryReadsAsMiss(t *testing.T) {
	c, mr := newTestClient(t, testTTL)

	if err := mr.Set(authKey("alice"), "{not json"); err != nil {
		t.Fatalf("seed corrupt value: %v", err)
	}

	_, hit, err := c.GetAuthCache(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetAuthCache: %v", err)
	}
	if hit {
		t.Error("corrupt entry reported as a hit")
	}
}

func TestUserCacheRoundTripAndAvatarURL(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	const rowVersion = 5
	user := &model.UserCached{Username: "alice", Nickname: "Alice", AvatarSuffix: "deadbeef"}
	if err := c.SetUserCache(ctx, 42, rowVersion, user); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	got, hit, err := c.GetUserCache(ctx, 42, rowVersion)
	if err != nil || !hit {
		t.Fatalf("GetUserCache: hit=%v err=%v", hit, err)
	}
	if got.Username != "alice" || got.Nickname != "Alice" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	// The URL is derived from the cached fields, so a hit and a miss must
	// produce byte-identical output.
	want := (&model.User{ID: 42, AvatarSuffix: "deadbeef"}).AvatarURL()
	if got.AvatarURL(42) != want {
		t.Errorf("avatar url = %q, want %q", got.AvatarURL(42), want)
	}
}

// The whole point of embedding row_version in the key: a stale guess is
// invisible to the caller as anything other than an ordinary cache miss, not
// an error and not stale data. This is what makes the scheme safe even though
// nothing validates the guess before trying it.
func TestUserCacheWrongVersionGuessIsJustAMiss(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	if err := c.SetUserCache(ctx, 42, 5, &model.UserCached{Username: "alice", Nickname: "Alice"}); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	// A caller guessing an old, a skipped, or a not-yet-reached version must
	// all see a plain miss — never the wrong user's data, never an error.
	for _, guess := range []uint64{0, 4, 6, 999} {
		if _, hit, err := c.GetUserCache(ctx, 42, guess); err != nil || hit {
			t.Errorf("guess v%d: hit=%v err=%v, want a clean miss", guess, hit, err)
		}
	}
}

// The two caches are keyed differently on purpose; a user record write must
// not be readable through the auth cache or vice versa.
func TestAuthAndUserKeysDoNotCollide(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	if err := c.SetAuthCache(ctx, "42", sampleAuth()); err != nil {
		t.Fatalf("SetAuthCache: %v", err)
	}

	if _, hit, _ := c.GetUserCache(ctx, 42, 5); hit {
		t.Error("auth:42 was readable as user:42:v5")
	}
}

// Nickname/avatar edits invalidate only user:<user_id>:v<N> now — auth:
// carries nothing a profile edit could make stale (see model.AuthCached), so
// it is correctly left untouched, not merely "also dropped for safety".
func TestInvalidateOnNicknameChangeLeavesAuthAlone(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	const oldRowVersion = 5
	if err := c.SetAuthCache(ctx, "alice", sampleAuth()); err != nil {
		t.Fatalf("SetAuthCache: %v", err)
	}
	if err := c.SetUserCache(ctx, 42, oldRowVersion, &model.UserCached{Username: "alice", Nickname: "Alice"}); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	if err := c.InvalidateOnNicknameChange(ctx, 42, oldRowVersion); err != nil {
		t.Fatalf("InvalidateOnNicknameChange: %v", err)
	}

	if !mr.Exists(authKey("alice")) {
		t.Error("auth:alice was invalidated by a nickname change; it should not be")
	}
	if mr.Exists(userKey(42, oldRowVersion)) {
		t.Error("user:42:v5 survived invalidation")
	}
}

// Deleting keys that were never cached is a no-op, which is what makes it safe
// to invalidate unconditionally after every write.
func TestInvalidateIsIdempotent(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	for range 2 {
		if err := c.InvalidateOnAvatarChange(ctx, 999, 3); err != nil {
			t.Fatalf("InvalidateOnAvatarChange on absent keys: %v", err)
		}
	}
}

// Unlike nickname/avatar invalidation, a password change targets auth: —
// there is no caller for this today (no change-password feature), but the
// key it deletes must still be the right one.
func TestInvalidateOnPasswordChangeDropsAuthNotUser(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	const rowVersion = 5
	if err := c.SetAuthCache(ctx, "alice", sampleAuth()); err != nil {
		t.Fatalf("SetAuthCache: %v", err)
	}
	if err := c.SetUserCache(ctx, 42, rowVersion, &model.UserCached{Username: "alice", Nickname: "Alice"}); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	if err := c.InvalidateOnPasswordChange(ctx, "alice"); err != nil {
		t.Fatalf("InvalidateOnPasswordChange: %v", err)
	}

	if mr.Exists(authKey("alice")) {
		t.Error("auth:alice survived a password change")
	}
	// user: never holds the hash, so dropping it would be pure cache churn.
	if !mr.Exists(userKey(42, rowVersion)) {
		t.Error("user:42:v5 was dropped by a password change")
	}
}

// CACHE_LAYER=off must produce a permanent miss and skip the write, so a
// "without cache" load-test run exercises MySQL on every single request.
func TestUserCacheDisabledAlwaysMisses(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := New(&config.RedisConfig{
		Addr: mr.Addr(), PoolSize: 10, UserCacheDisabled: true,
	}, testTTL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()

	if err := c.SetAuthCache(ctx, "alice", sampleAuth()); err != nil {
		t.Fatalf("SetAuthCache: %v", err)
	}
	// The write must not have happened at all; a key written now would start
	// serving hits as soon as the flag was flipped back.
	if mr.Exists(authKey("alice")) {
		t.Error("disabled cache still wrote auth:alice")
	}
	if _, hit, _ := c.GetAuthCache(ctx, "alice"); hit {
		t.Error("disabled cache reported a hit")
	}

	if err := c.SetUserCache(ctx, 42, 5, &model.UserCached{Username: "alice", Nickname: "Alice"}); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}
	if _, hit, _ := c.GetUserCache(ctx, 42, 5); hit {
		t.Error("disabled user cache reported a hit")
	}
}

// Disabling the auth/user cache must leave sessions working, or a "without
// cache" run measures the 401 path instead of the uncached one.
func TestUserCacheDisabledKeepsSessionsWorking(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := New(&config.RedisConfig{
		Addr: mr.Addr(), PoolSize: 10, UserCacheDisabled: true,
	}, testTTL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := context.Background()

	token, err := c.CreateSession(ctx, 42, "203.0.113.7")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	session, err := c.GetSession(ctx, token)
	if err != nil {
		t.Fatalf("GetSession with cache disabled: %v", err)
	}
	if session.UserID != 42 {
		t.Errorf("user id = %d, want 42", session.UserID)
	}
}

// The two TTLs differ on purpose: the record carrying the password hash expires
// quickly, the cosmetic one is allowed to live an hour.
func TestCacheTTLs(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	if err := c.SetAuthCache(ctx, "alice", sampleAuth()); err != nil {
		t.Fatalf("SetAuthCache: %v", err)
	}
	if err := c.SetUserCache(ctx, 42, 5, &model.UserCached{Username: "alice", Nickname: "Alice"}); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	// Just past the 10-minute auth TTL, well short of the 1-hour user TTL.
	mr.FastForward(c.authTTL + time.Second)

	if _, hit, _ := c.GetAuthCache(ctx, "alice"); hit {
		t.Errorf("auth record outlived its %s TTL", c.authTTL)
	}
	if _, hit, _ := c.GetUserCache(ctx, 42, 5); !hit {
		t.Errorf("user record expired before its %s TTL", c.userTTL)
	}
}
