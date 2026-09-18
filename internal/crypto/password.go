// Package crypto wraps bcrypt password hashing and secure random generation.
package crypto

import "golang.org/x/crypto/bcrypt"

// DefaultBcryptCost is the bcrypt cost factor used for password hashing.
const DefaultBcryptCost = 10

// HashPassword generates a bcrypt hash of the plaintext password.
func HashPassword(plain string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(plain), DefaultBcryptCost)
}

// HashPasswordCost generates a bcrypt hash at a specific cost.
func HashPasswordCost(plain string, cost int) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(plain), cost)
}

// VerifyPassword returns nil if the plaintext matches the hash, else an error.
func VerifyPassword(hash []byte, plain string) error {
	return bcrypt.CompareHashAndPassword(hash, []byte(plain))
}
