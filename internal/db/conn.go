// TODO: Per-connection wrapper with prepared statement cache (see §4.12).
//
// Each pooled MySQL connection owns its own cache of prepared statements,
// because MySQL prepares are per-connection.
//
// Structure:
//   type Conn struct {
//       raw       driver.Conn                 // from mysql.Connector
//       stmtCache map[string]*driver.Stmt     // SQL string → prepared stmt
//       cacheMu   sync.Mutex
//       lastUsed  time.Time
//       created   time.Time
//   }
//
// Public API:
//   func (c *Conn) Prepare(sql string) (*driver.Stmt, error)  // uses cache
//   func (c *Conn) Exec(sql string, args ...) (driver.Result, error)
//   func (c *Conn) Query(sql string, args ...) (driver.Rows, error)
//   func (c *Conn) Close() error   // returns to pool, not physically closed
//   func (c *Conn) Ping() error    // health check
//
// Prepared statements to expect (from §4.12):
//   - SELECT id, password_hash, nickname, avatar_version, avatar_suffix FROM users WHERE username = ?
//   - SELECT id, nickname, avatar_version, avatar_suffix FROM users WHERE id = ?
//   - UPDATE users SET nickname = ? WHERE id = ?
//   - UPDATE users SET avatar_version = LAST_INSERT_ID(avatar_version + 1), avatar_suffix = ? WHERE id = ?
//   - INSERT INTO users (username, password_hash, nickname) VALUES (?, ?, ?)
package db
