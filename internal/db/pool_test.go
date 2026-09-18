// TODO: Tests for the custom MySQL pool.
//
// Coverage to add:
//   - Concurrent Acquire from many goroutines respects max_open
//   - Acquire blocks when pool exhausted, returns on Release
//   - Acquire returns error if wait timeout exceeded
//   - Health check evicts closed/broken connections
//   - Shutdown drains in-flight and rejects new Acquires
//   - Prepared statement cache: same SQL string returns same handle per connection
//   - Prepared statement cache: different connections have independent caches
//
// May require a real MySQL instance (docker) or a mock connector.
package db
