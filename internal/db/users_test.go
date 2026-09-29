package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/crypto"
)

// These are integration tests: they run real SQL against a real MySQL. There is
// nothing worth unit testing here, because every function in users.go is a
// claim about what MySQL does, and a mock would only assert that the same query
// string was typed twice.
//
// They run against userdb_test, never the seeded userdb. Set up once with:
//
//	sudo mysql -u root -e "CREATE DATABASE userdb_test CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; \
//	  GRANT ALL PRIVILEGES ON userdb_test.* TO 'app'@'%';"
//	mysql -h 127.0.0.1 -u app -papp userdb_test < schema.sql
const defaultTestDSN = "app:app@tcp(127.0.0.1:3306)/userdb_test?parseTime=true&charset=utf8mb4"

func testHandle(t *testing.T) Handle {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping MySQL integration test in -short mode")
	}

	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		dsn = defaultTestDSN
	}

	h, err := Open(context.Background(), &config.MySQLConfig{DSN: dsn, MaxOpen: 16})
	if err != nil {
		t.Skipf("no MySQL test database reachable (%v); see the setup comment in users_test.go", err)
	}
	t.Cleanup(func() { h.Close() })

	return h
}

// insertTestUser creates a uniquely named user and schedules its removal, so
// tests never collide and never leave rows behind.
func insertTestUser(t *testing.T, h Handle, nickname string) (uint64, string, []byte) {
	t.Helper()

	suffix, err := crypto.RandomHex(8) // 16 chars, comfortably inside VARCHAR(32)
	if err != nil {
		t.Fatalf("RandomHex: %v", err)
	}
	username := "test_" + suffix
	hash := []byte(fmt.Sprintf("$2a$10$fakehashfortesting%s", suffix))

	id, err := InsertUser(context.Background(), h, username, hash, nickname)
	if err != nil {
		t.Fatalf("InsertUser: %v", err)
	}
	t.Cleanup(func() {
		h.ExecContext(context.Background(), "DELETE FROM users WHERE id = ?", id)
	})

	return uint64(id), username, hash
}

// TestInsertAndGetUserByUsername is the core store-then-query round trip.
func TestInsertAndGetUserByUsername(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	id, username, hash := insertTestUser(t, h, "Test User")

	got, err := GetUserByUsername(ctx, h, username)
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}

	if got.ID != id {
		t.Errorf("ID = %d, want %d", got.ID, id)
	}
	if got.Username != username {
		t.Errorf("Username = %q, want %q", got.Username, username)
	}
	if string(got.PasswordHash) != string(hash) {
		t.Errorf("PasswordHash = %q, want %q", got.PasswordHash, hash)
	}
	if got.Nickname != "Test User" {
		t.Errorf("Nickname = %q, want %q", got.Nickname, "Test User")
	}
	// A fresh user has no avatar. avatar_suffix is NULL in MySQL and must
	// arrive as "" rather than blowing up the scan.
	if got.AvatarSuffix != "" {
		t.Errorf("AvatarSuffix = %q, want \"\" for a NULL column", got.AvatarSuffix)
	}
	if got.RowVersion != 0 {
		t.Errorf("RowVersion = %d, want 0 for a row that has never been updated", got.RowVersion)
	}
}

func TestGetUserByID(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	id, username, _ := insertTestUser(t, h, "By ID")

	got, err := GetUserByID(ctx, h, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.ID != id || got.Username != username || got.Nickname != "By ID" {
		t.Errorf("got %+v, want id=%d username=%q nickname=%q", got, id, username, "By ID")
	}
}

// TestGetUserNotFound pins the error translation: callers must see the
// package's own sentinel, never sql.ErrNoRows.
func TestGetUserNotFound(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	if _, err := GetUserByUsername(ctx, h, "definitely_no_such_user"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("GetUserByUsername error = %v, want ErrUserNotFound", err)
	}
	if _, err := GetUserByID(ctx, h, 1<<62); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("GetUserByID error = %v, want ErrUserNotFound", err)
	}
}

// TestInsertDuplicateUsername covers the other half of the translation: MySQL
// error 1062 from uk_username becomes ErrDuplicateUsername, which is what the
// signup handler turns into "username taken".
func TestInsertDuplicateUsername(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	_, username, hash := insertTestUser(t, h, "First")

	_, err := InsertUser(ctx, h, username, hash, "Second")
	if !errors.Is(err, ErrDuplicateUsername) {
		t.Errorf("InsertUser error = %v, want ErrDuplicateUsername", err)
	}
}

// TestUsernameIsCaseSensitive pins a schema decision rather than Go code: the
// utf8mb4_bin collation means Alice and alice are different accounts. Flipping
// the column to a _ci collation would silently start rejecting signups as
// duplicates, and this is the test that would catch it.
func TestUsernameIsCaseSensitive(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	_, username, hash := insertTestUser(t, h, "lower")

	upper := "TEST_" + username[5:]
	id, err := InsertUser(ctx, h, upper, hash, "upper")
	if err != nil {
		t.Fatalf("InsertUser(%q) after %q: %v; the username collation is not binary", upper, username, err)
	}
	t.Cleanup(func() { h.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id) })

	if _, err := GetUserByUsername(ctx, h, upper); err != nil {
		t.Errorf("GetUserByUsername(%q): %v", upper, err)
	}
}

func TestUpdateNickname(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	id, username, _ := insertTestUser(t, h, "Before")

	// An emoji doubles as a check that the column really is utf8mb4 and not
	// MySQL's 3-byte "utf8".
	const want = "Renamed 🦊"
	newRowVersion, err := UpdateNickname(ctx, h, id, want)
	if err != nil {
		t.Fatalf("UpdateNickname: %v", err)
	}
	if newRowVersion != 1 {
		t.Errorf("new row_version = %d, want 1 (first mutating write on a fresh row)", newRowVersion)
	}

	got, err := GetUserByUsername(ctx, h, username)
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got.Nickname != want {
		t.Errorf("Nickname = %q, want %q", got.Nickname, want)
	}
	if got.RowVersion != newRowVersion {
		t.Errorf("stored row_version = %d, want %d (what UpdateNickname returned)", got.RowVersion, newRowVersion)
	}
}

func TestUpdateAvatar(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	id, _, _ := insertTestUser(t, h, "Avatar")

	rv1, err := UpdateAvatar(ctx, h, id, "deadbeef")
	if err != nil {
		t.Fatalf("UpdateAvatar: %v", err)
	}
	if rv1 != 1 {
		t.Errorf("first row_version = %d, want 1 (first mutating write on a fresh row)", rv1)
	}

	rv2, err := UpdateAvatar(ctx, h, id, "cafef00d")
	if err != nil {
		t.Fatalf("UpdateAvatar: %v", err)
	}
	if rv2 != 2 {
		t.Errorf("second row_version = %d, want 2", rv2)
	}

	got, err := GetUserByID(ctx, h, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	// Only the suffix identifies the current avatar now; the second upload's
	// suffix must have overwritten the first's, with no version column to
	// check.
	if got.AvatarSuffix != "cafef00d" {
		t.Errorf("stored avatar suffix = %q, want %q", got.AvatarSuffix, "cafef00d")
	}
	if got.RowVersion != 2 {
		t.Errorf("stored row_version = %d, want 2 after two uploads", got.RowVersion)
	}

	wantURL := fmt.Sprintf("/uploads/%d/%d/%d_cafef00d.jpg", id>>14, id, id)
	if got.AvatarURL() != wantURL {
		t.Errorf("AvatarURL() = %q, want %q", got.AvatarURL(), wantURL)
	}
}

func TestUpdateAvatarUnknownUser(t *testing.T) {
	h := testHandle(t)

	if _, err := UpdateAvatar(context.Background(), h, 1<<62, "deadbeef"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("UpdateAvatar error = %v, want ErrUserNotFound", err)
	}
}

// TestUpdateAvatarRowVersionIsAtomic is what justifies the LAST_INSERT_ID
// trick now that avatar_version is gone and row_version is the only counter
// left in this statement: a SELECT-then-UPDATE implementation would let
// concurrent uploads read the same row_version and hand out duplicates,
// which breaks the invalidation scheme in §4.1 (two uploads would both
// believe they are invalidating the same "old" key, and the actually-current
// key from whichever won would never get a distinct one to be invalidated
// from later). Uniqueness of the FILE, unlike in the old avatar_version
// scheme, no longer depends on this at all — that now rests entirely on the
// random suffix never repeating for the same user, which this test does not
// need to (and cannot usefully) check.
func TestUpdateAvatarRowVersionIsAtomic(t *testing.T) {
	h := testHandle(t)
	ctx := context.Background()

	id, _, _ := insertTestUser(t, h, "Concurrent")

	const n = 16
	rowVersions := make([]uint64, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			rowVersions[i], errs[i] = UpdateAvatar(ctx, h, id, fmt.Sprintf("%08x", i))
		}()
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("UpdateAvatar #%d: %v", i, err)
		}
		if seen[rowVersions[i]] {
			t.Errorf("row_version %d handed out more than once", rowVersions[i])
		}
		seen[rowVersions[i]] = true
	}
	for want := uint64(1); want <= n; want++ {
		if !seen[want] {
			t.Errorf("row_version %d was never returned; row_versions are not a clean sequence", want)
		}
	}
}
