// seed populates the users table with N rows for perf testing.
//
// Usage:
//   go run ./cmd/seed --rows 10000000 --dsn "app:app@tcp(127.0.0.1:3306)/userdb"
//
// All seeded users share the same bcrypt hash (of password "testpass") to avoid
// hashing 10M times. Usernames are "user000000001", "user000000002", etc.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"entry_profile_creation/internal/crypto"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	rows := flag.Int("rows", 10_000_000, "number of user rows to insert")
	dsn := flag.String("dsn", "app:app@tcp(127.0.0.1:3306)/userdb?charset=utf8mb4", "MySQL DSN")
	batch := flag.Int("batch", 1000, "rows per INSERT batch")
	flag.Parse()

	if *rows <= 0 {
		log.Fatal("--rows must be > 0")
	}
	if *batch <= 0 || *batch > 10000 {
		log.Fatal("--batch must be 1-10000")
	}

	log.Printf("seeding %d rows into %s (batch=%d)", *rows, *dsn, *batch)

	// Pre-compute one bcrypt hash for all users
	hash, err := crypto.HashPasswordCost("testpass", 10)
	if err != nil {
		log.Fatalf("hash: %v", err)
	}

	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)

	if err := db.Ping(); err != nil {
		log.Fatalf("ping: %v", err)
	}

	start := time.Now()
	inserted := 0

	// Build a reusable placeholder template
	placeholderRow := "(?, ?, ?)"
	for base := 1; base <= *rows; base += *batch {
		end := base + *batch - 1
		if end > *rows {
			end = *rows
		}
		n := end - base + 1

		// Build "(?, ?, ?), (?, ?, ?), ..."
		var sb strings.Builder
		sb.WriteString("INSERT INTO users (username, password_hash, nickname) VALUES ")
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(placeholderRow)
		}

		args := make([]any, 0, n*3)
		for i := 0; i < n; i++ {
			username := fmt.Sprintf("user%09d", base+i)
			args = append(args, username, hash, username)
		}

		if _, err := db.Exec(sb.String(), args...); err != nil {
			log.Fatalf("insert batch base=%d: %v", base, err)
		}
		inserted += n

		if inserted%100_000 == 0 || inserted == *rows {
			elapsed := time.Since(start)
			rate := float64(inserted) / elapsed.Seconds()
			log.Printf("inserted %d/%d (%.0f rows/sec, elapsed %s)", inserted, *rows, rate, elapsed.Truncate(time.Second))
		}
	}
	log.Printf("done: %d rows in %s", inserted, time.Since(start).Truncate(time.Second))
}
