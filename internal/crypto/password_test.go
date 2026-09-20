package crypto

import (
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// testCost is bcrypt's minimum. The production cost of 10 takes ~75ms per
// operation by design, which would make this file take seconds for no extra
// coverage — the cost parameter is not what is under test here.
const testCost = bcrypt.MinCost

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse battery"

	hash, err := HashPasswordWithCost(password, testCost)
	if err != nil {
		t.Fatalf("HashPasswordWithCost: %v", err)
	}
	if len(hash) != 60 {
		t.Errorf("hash length = %d, want 60 to match VARBINARY(60)", len(hash))
	}
	if string(hash) == password {
		t.Fatal("hash is the plaintext")
	}

	if err := VerifyPassword(hash, []byte(password)); err != nil {
		t.Errorf("VerifyPassword with correct password = %v, want nil", err)
	}
	if err := VerifyPassword(hash, []byte("wrong password")); !errors.Is(err, ErrPasswordMismatch) {
		t.Errorf("VerifyPassword with wrong password = %v, want ErrPasswordMismatch", err)
	}
}

// TestHashIsSalted is the property that makes a stolen users table hard to
// attack in bulk: identical passwords produce different hashes, so an attacker
// cannot spot shared passwords or precompute a rainbow table.
func TestHashIsSalted(t *testing.T) {
	const password = "correct horse battery"

	first, err := HashPasswordWithCost(password, testCost)
	if err != nil {
		t.Fatalf("HashPasswordWithCost: %v", err)
	}
	second, err := HashPasswordWithCost(password, testCost)
	if err != nil {
		t.Fatalf("HashPasswordWithCost: %v", err)
	}

	if string(first) == string(second) {
		t.Error("hashing the same password twice produced identical output; the salt is not random")
	}
	// Both must still verify — the salt is stored inside the hash itself.
	for i, h := range [][]byte{first, second} {
		if err := VerifyPassword(h, []byte(password)); err != nil {
			t.Errorf("hash %d failed to verify: %v", i, err)
		}
	}
}

// TestVerifyDistinguishesCorruptHash checks that a malformed hash in the
// database reports as a real error rather than as a wrong password, so login
// returns 500 and gets investigated instead of silently locking a user out.
func TestVerifyDistinguishesCorruptHash(t *testing.T) {
	err := VerifyPassword([]byte("not a bcrypt hash at all"), []byte("whatever"))
	if err == nil {
		t.Fatal("VerifyPassword with a corrupt hash = nil, want an error")
	}
	if errors.Is(err, ErrPasswordMismatch) {
		t.Error("a corrupt hash was reported as a password mismatch")
	}
}

// TestDummyVerifyUsesCachedHash covers the timing-equalisation helper. It must
// not panic, and repeated calls at the same cost must reuse the cached hash
// rather than generating a new one each time.
func TestDummyVerifyUsesCachedHash(t *testing.T) {
	DummyVerify(testCost)

	first, ok := dummyHashes.Load(testCost)
	if !ok {
		t.Fatal("DummyVerify did not cache a hash")
	}

	DummyVerify(testCost)
	second, _ := dummyHashes.Load(testCost)
	if string(first.([]byte)) != string(second.([]byte)) {
		t.Error("DummyVerify regenerated the hash; it should cost one compare, not a generate plus a compare")
	}
}
