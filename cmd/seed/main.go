// TODO: One-time seeder to insert 10M user rows into MySQL for perf testing.
//
// Responsibilities:
//   - Connect to MySQL (using the standard driver, or the custom pool for consistency)
//   - Batch-insert 10M rows into the users table:
//       - username: deterministic pattern like "user000001", "user000002", ...
//       - password_hash: pre-computed bcrypt hash of a fixed password (avoid hashing 10M times)
//       - nickname: mirror of username or a template
//       - avatar_version = 0, avatar_suffix = NULL (defaults)
//   - Use bulk INSERT (multi-row) or LOAD DATA INFILE for speed
//   - Report progress every N rows
//   - Idempotent-friendly (skip on unique constraint violation, or wipe before)
//
// Build:  go build ./cmd/seed
// Run:    ./seed --rows 10000000 --dsn "user:pass@tcp(localhost:3306)/dbname"
package main

func main() {
	// TODO: implement
}
