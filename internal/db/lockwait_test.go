package db

import (
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// No MySQL needed: this only checks the session variables handed to the
// driver, which sends them as SET on every new connection.

func TestSetLockWaitTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
		d    time.Duration
		want map[string]string
	}{
		{"default", "u:p@tcp(h:3306)/db", 0,
			map[string]string{"lock_wait_timeout": "1", "innodb_lock_wait_timeout": "1"}},
		{"configured", "u:p@tcp(h:3306)/db", 3 * time.Second,
			map[string]string{"lock_wait_timeout": "3", "innodb_lock_wait_timeout": "3"}},
		{"sub-second rounds up to 1", "u:p@tcp(h:3306)/db", 200 * time.Millisecond,
			map[string]string{"lock_wait_timeout": "1", "innodb_lock_wait_timeout": "1"}},
		{"rounds up to whole seconds", "u:p@tcp(h:3306)/db", 1500 * time.Millisecond,
			map[string]string{"lock_wait_timeout": "2", "innodb_lock_wait_timeout": "2"}},
		{"DSN value wins", "u:p@tcp(h:3306)/db?innodb_lock_wait_timeout=2", 0,
			map[string]string{"lock_wait_timeout": "1", "innodb_lock_wait_timeout": "2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := mysql.ParseDSN(tc.dsn)
			if err != nil {
				t.Fatalf("ParseDSN: %v", err)
			}
			setLockWaitTimeouts(cfg, tc.d)
			for k, v := range tc.want {
				if got := cfg.Params[k]; got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
		})
	}
}
