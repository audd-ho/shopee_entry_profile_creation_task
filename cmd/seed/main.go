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

// ---------------------------------------------------------------------------
// Phase 2 runs this with --rows 10000 for fast iteration; phase 6 runs the same
// binary with --rows 10000000 overnight. Nothing here changes between the two.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"

	"entry_profile_creation/internal/config"
)

const (
	// seedPassword is the plaintext every seeded account shares. Seed data is
	// for load testing, not for holding secrets.
	seedPassword = "password123"

	// columnsPerRow is the number of placeholders each row contributes.
	columnsPerRow = 3

	// progressEvery bounds how often progress is logged, independent of batch
	// size, so a small batch does not drown the output.
	progressEvery = 50_000
)

type options struct {
	dsn      string
	rows     int
	batch    int
	start    int
	cost     int
	truncate bool
	ignore   bool
}

func main() {
	cfg := config.Load()

	var opts options
	flag.StringVar(&opts.dsn, "dsn", cfg.MySQL.DSN, "MySQL DSN")
	flag.IntVar(&opts.rows, "rows", 10_000, "number of users to insert")
	flag.IntVar(&opts.batch, "batch", 1_000, "rows per INSERT statement")
	flag.IntVar(&opts.start, "start", 1, "first user index, for resuming a partial run")
	flag.IntVar(&opts.cost, "bcrypt-cost", cfg.Bcrypt.Cost, "bcrypt cost for the shared hash")
	flag.BoolVar(&opts.truncate, "truncate", false, "TRUNCATE users before seeding")
	flag.BoolVar(&opts.ignore, "ignore", false, "use INSERT IGNORE so duplicate usernames are skipped")
	flag.Parse()

	if opts.rows <= 0 || opts.batch <= 0 {
		log.Fatal("seed: --rows and --batch must be positive")
	}

	// A 10M run takes hours. Honour Ctrl-C so it stops between batches with the
	// committed rows intact, rather than being killed mid-transaction.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, opts); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

func run(ctx context.Context, opts options) error {
	db, err := sql.Open("mysql", opts.dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	// The loop below is single threaded, so a couple of connections is plenty.
	// Parallel writers are a phase 6 concern, once the row count justifies it.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	// The single most important line in this program. bcrypt at cost 10 takes
	// ~75ms by design, so hashing per row would cost ~12 minutes at 10k rows
	// and over a week at 10M. Every seeded account therefore shares one hash;
	// real signups hash for real.
	hash, err := bcrypt.GenerateFromPassword([]byte(seedPassword), opts.cost)
	if err != nil {
		return fmt.Errorf("bcrypt: %w", err)
	}
	log.Printf("precomputed one bcrypt hash at cost %d, reused for all %d rows", opts.cost, opts.rows)

	if opts.truncate {
		log.Print("truncating users")
		if _, err := db.ExecContext(ctx, "TRUNCATE TABLE users"); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
	}

	insertVerb := "INSERT INTO"
	if opts.ignore {
		insertVerb = "INSERT IGNORE INTO"
	}

	var (
		started    = time.Now()
		inserted   int
		lastLogged int
	)
	for offset := 0; offset < opts.rows; offset += opts.batch {
		n := min(opts.batch, opts.rows-offset)
		if err := insertBatch(ctx, db, insertVerb, opts.start+offset, n, hash); err != nil {
			return fmt.Errorf("batch starting at index %d: %w", opts.start+offset, err)
		}
		inserted += n

		if inserted-lastLogged >= progressEvery || inserted == opts.rows {
			elapsed := time.Since(started)
			log.Printf("seeded %d/%d rows in %s (%.0f rows/s)",
				inserted, opts.rows, elapsed.Truncate(time.Millisecond),
				float64(inserted)/elapsed.Seconds())
			lastLogged = inserted
		}
	}

	log.Printf("done: %d rows in %s", inserted, time.Since(started).Truncate(time.Millisecond))
	return nil
}

// insertBatch writes n users in a single multi-row INSERT inside its own
// transaction.
//
// Two separate wins. Multi-row INSERT collapses n network round trips into one,
// which is what makes 10M feasible at all. Committing per batch rather than
// wrapping the whole run in one transaction keeps the InnoDB undo log bounded:
// a single 10M-row transaction would balloon it and end up slower than the
// naive version, as well as being unresumable.
func insertBatch(ctx context.Context, db *sql.DB, insertVerb string, firstIndex, n int, hash []byte) error {
	// n*3 placeholders at ~60 bytes of arguments per row. At the default batch
	// of 1000 that is well under MySQL's 64MB max_allowed_packet; a much larger
	// --batch would need that checked.
	var sb strings.Builder
	sb.WriteString(insertVerb)
	sb.WriteString(" users (username, password_hash, nickname) VALUES ")

	args := make([]any, 0, n*columnsPerRow)
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("(?,?,?)")
		// Deterministic names make the run resumable via --start, and give the
		// load tests in phase 6 a known set of keys to query.
		name := fmt.Sprintf("user%08d", firstIndex+i)
		args = append(args, name, hash, name)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// No-op once Commit has succeeded; guarantees no leaked transaction on any
	// early return.
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return tx.Commit()
}
