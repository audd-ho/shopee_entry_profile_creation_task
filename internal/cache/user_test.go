package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/model"
)

// The user and profile caches are exercised against miniredis for the same
// reasons the session tests are: no server to install, and a clock that can be
// fast-forwarded to check TTLs without sleeping for an hour.

func sampleUser() *model.UserCached {
	return &model.UserCached{
		ID:            42,
		PasswordHash:  []byte("$2a$10$abcdefghijklmnopqrstuv"),
		Nickname:      "Alice",
		AvatarVersion: 3,
		AvatarSuffix:  "deadbeef",
		RowVersion:    5,
	}
}

func TestUserCacheRoundTrip(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	if err := c.SetUserCache(ctx, "alice", sampleUser()); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	got, hit, err := c.GetUserCache(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserCache: %v", err)
	}
	if !hit {
		t.Fatal("expected a hit for a key just written")
	}
	// The hash in particular must survive the round trip: it is what lets a
	// cache hit verify a password without touching MySQL.
	if string(got.PasswordHash) != string(sampleUser().PasswordHash) {
		t.Errorf("password hash = %q, want %q", got.PasswordHash, sampleUser().PasswordHash)
	}
	if got.ID != 42 || got.Nickname != "Alice" || got.AvatarVersion != 3 {
		t.Errorf("round trip lost fields: %+v", got)
	}
	// RowVersion must survive too: it is what lets a cache-hit login seed an
	// accurate hint on the new session, instead of always seeding 0 and
	// eating a guaranteed miss on that session's first profile read.
	if got.RowVersion != 5 {
		t.Errorf("RowVersion = %d, want 5", got.RowVersion)
	}
}

// A miss must be reported as hit=false with a nil error, never as an error.
// Conflating the two would make an uncached user look like a broken Redis, or
// worse, like a user that does not exist.
func TestUserCacheMissIsNotAnError(t *testing.T) {
	c, _ := newTestClient(t, testTTL)

	got, hit, err := c.GetUserCache(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("GetUserCache on a missing key returned an error: %v", err)
	}
	if hit || got != nil {
		t.Errorf("hit = %v, got = %+v; want false, nil", hit, got)
	}
}

// A malformed entry is treated as a miss so the caller falls through to MySQL
// and overwrites it, rather than one bad key breaking a user until it expires.
func TestUserCacheCorruptEntryReadsAsMiss(t *testing.T) {
	c, mr := newTestClient(t, testTTL)

	if err := mr.Set(userKey("alice"), "{not json"); err != nil {
		t.Fatalf("seed corrupt value: %v", err)
	}

	_, hit, err := c.GetUserCache(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetUserCache: %v", err)
	}
	if hit {
		t.Error("corrupt entry reported as a hit")
	}
}

func TestProfileCacheRoundTripAndAvatarURL(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	const rowVersion = 5
	profile := &model.ProfileCached{Nickname: "Alice", AvatarVersion: 3, AvatarSuffix: "deadbeef"}
	if err := c.SetProfileCache(ctx, 42, rowVersion, profile); err != nil {
		t.Fatalf("SetProfileCache: %v", err)
	}

	got, hit, err := c.GetProfileCache(ctx, 42, rowVersion)
	if err != nil || !hit {
		t.Fatalf("GetProfileCache: hit=%v err=%v", hit, err)
	}
	// The URL is derived from the cached fields, so a hit and a miss must
	// produce byte-identical output.
	want := (&model.User{ID: 42, AvatarVersion: 3, AvatarSuffix: "deadbeef"}).AvatarURL()
	if got.AvatarURL(42) != want {
		t.Errorf("avatar url = %q, want %q", got.AvatarURL(42), want)
	}
}

// The whole point of embedding row_version in the key: a stale guess is
// invisible to the caller as anything other than an ordinary cache miss, not
// an error and not stale data. This is what makes the scheme safe even though
// nothing validates the guess before trying it.
func TestProfileCacheWrongVersionGuessIsJustAMiss(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	if err := c.SetProfileCache(ctx, 42, 5, &model.ProfileCached{Nickname: "Alice"}); err != nil {
		t.Fatalf("SetProfileCache: %v", err)
	}

	// A caller guessing an old, a skipped, or a not-yet-reached version must
	// all see a plain miss — never the wrong user's data, never an error.
	for _, guess := range []uint64{0, 4, 6, 999} {
		if _, hit, err := c.GetProfileCache(ctx, 42, guess); err != nil || hit {
			t.Errorf("guess v%d: hit=%v err=%v, want a clean miss", guess, hit, err)
		}
	}
}

// The two caches are keyed differently on purpose; a profile write must not be
// readable as a user record or vice versa.
func TestUserAndProfileKeysDoNotCollide(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	if err := c.SetUserCache(ctx, "42", sampleUser()); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}

	if _, hit, _ := c.GetProfileCache(ctx, 42, 5); hit {
		t.Error("user:42 was readable as profile:42:v5")
	}
}

func TestInvalidateOnNicknameChangeDropsBothKeys(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	const oldRowVersion = 5
	if err := c.SetUserCache(ctx, "alice", sampleUser()); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}
	if err := c.SetProfileCache(ctx, 42, oldRowVersion, &model.ProfileCached{Nickname: "Alice"}); err != nil {
		t.Fatalf("SetProfileCache: %v", err)
	}

	if err := c.InvalidateOnNicknameChange(ctx, 42, oldRowVersion, "alice"); err != nil {
		t.Fatalf("InvalidateOnNicknameChange: %v", err)
	}

	// Both, not just profile:. user: is keyed by username and read by login,
	// but it carries a nickname copy that would otherwise go stale.
	if mr.Exists(userKey("alice")) {
		t.Error("user:alice survived invalidation")
	}
	if mr.Exists(profileKey(42, oldRowVersion)) {
		t.Error("profile:42:v5 survived invalidation")
	}
}

// Deleting keys that were never cached is a no-op, which is what makes it safe
// to invalidate unconditionally after every write.
func TestInvalidateIsIdempotent(t *testing.T) {
	c, _ := newTestClient(t, testTTL)
	ctx := context.Background()

	for range 2 {
		if err := c.InvalidateOnAvatarChange(ctx, 999, 3, "ghost"); err != nil {
			t.Fatalf("InvalidateOnAvatarChange on absent keys: %v", err)
		}
	}
}

func TestInvalidateOnPasswordChangeSparesProfile(t *testing.T) {
	c, mr := newTestClient(t, testTTL)
	ctx := context.Background()

	const rowVersion = 5
	if err := c.SetUserCache(ctx, "alice", sampleUser()); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}
	if err := c.SetProfileCache(ctx, 42, rowVersion, &model.ProfileCached{Nickname: "Alice"}); err != nil {
		t.Fatalf("SetProfileCache: %v", err)
	}

	if err := c.InvalidateOnPasswordChange(ctx, "alice"); err != nil {
		t.Fatalf("InvalidateOnPasswordChange: %v", err)
	}

	if mr.Exists(userKey("alice")) {
		t.Error("user:alice survived a password change")
	}
	// profile: never holds the hash, so dropping it would be pure cache churn.
	if !mr.Exists(profileKey(42, rowVersion)) {
		t.Error("profile:42:v5 was dropped by a password change")
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

	if err := c.SetUserCache(ctx, "alice", sampleUser()); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}
	// The write must not have happened at all; a key written now would start
	// serving hits as soon as the flag was flipped back.
	if mr.Exists(userKey("alice")) {
		t.Error("disabled cache still wrote user:alice")
	}
	if _, hit, _ := c.GetUserCache(ctx, "alice"); hit {
		t.Error("disabled cache reported a hit")
	}

	if err := c.SetProfileCache(ctx, 42, 5, &model.ProfileCached{Nickname: "Alice"}); err != nil {
		t.Fatalf("SetProfileCache: %v", err)
	}
	if _, hit, _ := c.GetProfileCache(ctx, 42, 5); hit {
		t.Error("disabled profile cache reported a hit")
	}
}

// Disabling the user cache must leave sessions working, or a "without cache"
// run measures the 401 path instead of the uncached one.
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

	token, err := c.CreateSession(ctx, 42, 5, "alice", "203.0.113.7")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	session, err := c.GetSession(ctx, token)
	if err != nil {
		t.Fatalf("GetSession with user cache disabled: %v", err)
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

	if err := c.SetUserCache(ctx, "alice", sampleUser()); err != nil {
		t.Fatalf("SetUserCache: %v", err)
	}
	if err := c.SetProfileCache(ctx, 42, 5, &model.ProfileCached{Nickname: "Alice"}); err != nil {
		t.Fatalf("SetProfileCache: %v", err)
	}

	// Just past the 10-minute user TTL, well short of the 1-hour profile TTL.
	mr.FastForward(userTTL + time.Second)

	if _, hit, _ := c.GetUserCache(ctx, "alice"); hit {
		t.Errorf("user record outlived its %s TTL", userTTL)
	}
	if _, hit, _ := c.GetProfileCache(ctx, 42, 5); !hit {
		t.Errorf("profile record expired before its %s TTL", profileTTL)
	}
}
