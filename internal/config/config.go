// TODO: Configuration loading.
//
// Reads config from env vars with sensible defaults.
//
// type Config struct {
//     HTTP struct {
//         Addr string  // default ":8080"
//         TCPPoolSize int  // conns to TCP server; default 8
//     }
//     TCP struct {
//         Addr string  // default ":9000"
//     }
//     MySQL struct {
//         DSN      string  // required, e.g. "user:pass@tcp(host:3306)/db"
//         MaxOpen  int     // default 50
//     }
//     Redis struct {
//         Addr     string  // default "localhost:6379"
//         Password string
//         DB       int
//         PoolSize int     // default 50
//     }
//     Uploads struct {
//         Root       string  // default "./uploads"
//         MaxBytes   int64   // default 2*1024*1024 (2MB)
//     }
//     Bcrypt struct {
//         Cost int  // default 10
//     }
// }
//
// Public API:
//   func Load() (*Config, error)  // reads env, applies defaults, validates
package config
