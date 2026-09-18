// TODO: user-table queries.
//
// Typed query helpers that use the custom pool and prepared statements.
//
// Functions:
//   - GetUserByUsername(ctx, pool, username) (*model.User, error)
//   - GetUserByID(ctx, pool, id) (*model.User, error)
//   - InsertUser(ctx, pool, username, passwordHash, nickname) (int64, error)
//       Handle duplicate-key error → return ErrDuplicateUsername
//   - UpdateNickname(ctx, pool, id, nickname) error
//   - UpdateAvatar(ctx, pool, id, suffix) (newVersion uint32, err error)
//       Atomic increment + set suffix via LAST_INSERT_ID trick
//
// All queries use parameterized SQL (?)  — never string-concat.
package db
