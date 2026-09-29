// TODO: Username lookup table helpers (see sharding_design.md §8.3).
//
// The username_lookup table on the lookup shards is the global source of truth
// for username uniqueness. Each function takes a *Pool (the specific lookup
// shard, chosen by the caller via ShardedHandle.LookupShardFor).
package db

import (
	"context"
	"errors"
	"fmt"
)

const (
	qInsertLookup = `INSERT INTO username_lookup (username, user_id) VALUES (?, ?)`
	qGetLookup    = `SELECT user_id FROM username_lookup WHERE username = ?`
	qDeleteLookup = `DELETE FROM username_lookup WHERE username = ?`
)

// InsertLookup reserves a username→userID mapping on the lookup shard.
// Returns ErrDuplicateUsername if the username is already taken (PK collision).
func InsertLookup(ctx context.Context, h *Pool, username string, userID uint64) error {
	_, err := h.ExecContext(ctx, qInsertLookup, username, userID)
	if err != nil {
		if isDuplicateKey(err) {
			return ErrDuplicateUsername
		}
		return fmt.Errorf("db: insert lookup: %w", err)
	}
	return nil
}

// GetLookup resolves a username to its userID from the lookup shard.
// Returns ErrUserNotFound if the username does not exist.
func GetLookup(ctx context.Context, h *Pool, username string) (uint64, error) {
	var userID uint64
	err := h.QueryRowContext(ctx, qGetLookup, username).Scan(&userID)
	switch {
	case errors.Is(err, ErrNoRows):
		return 0, ErrUserNotFound
	case err != nil:
		return 0, fmt.Errorf("db: get lookup: %w", err)
	}
	return userID, nil
}

// DeleteLookup removes a username reservation. Used to roll back a lookup
// insert when the subsequent data-shard insert fails during signup.
func DeleteLookup(ctx context.Context, h *Pool, username string) error {
	_, err := h.ExecContext(ctx, qDeleteLookup, username)
	if err != nil {
		return fmt.Errorf("db: delete lookup: %w", err)
	}
	return nil
}
