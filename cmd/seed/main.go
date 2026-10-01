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
// Run:    ./seed --rows 10000000   (shard DSNs come from DATA_SHARD_DSNS / LOOKUP_SHARD_DSNS)
package main

// ---------------------------------------------------------------------------
// Phase 2 runs this with --rows 10000 for fast iteration; phase 6 runs the same
// binary with --rows 10000000 overnight. Nothing here changes between the two.
//
// Sharded layout (sharding_design.md §8). Every user writes two rows:
//   - users           on the data shard   chosen by ring(user_id)
//   - username_lookup on the lookup shard chosen by ring(username)
// The rings are built exactly as db.NewShardedHandle builds them (same shard
// counts, same vnodes), otherwise the server would look for rows on the wrong
// shard.
//
// Even data shards. Consistent hashing only gets *close* to an even split, but
// the seeder is free to choose user_ids, so it makes the split exact: user i
// is assigned to data shard (i-1) % N, and gets the next ID that the ring maps
// to that shard. Usernames stay contiguous (user00000001..userN) because the
// load tests pick random names in that range, so the lookup split is whatever
// ring(username) gives, which is close to but not exactly even.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"

	"entry_profile_creation/internal/config"
	"entry_profile_creation/internal/sharding"
)

const (
	// seedPassword is the plaintext every seeded account shares. Seed data is
	// for load testing, not for holding secrets.
	seedPassword = "password123"

	// progressEvery bounds how often progress is logged, independent of batch
	// size, so a small batch does not drown the output.
	progressEvery = 50_000
)

type options struct {
	dataDSNs   []string
	lookupDSNs []string
	vnodes     int
	rows       int
	batch      int
	start      int
	cost       int
	truncate   bool
	ignore     bool
}

func main() {
	cfg := config.Load()

	var (
		opts       options
		dataDSNs   string
		lookupDSNs string
	)
	flag.StringVar(&dataDSNs, "data-dsns", strings.Join(cfg.Shard.DataShardDSNs, ","), "comma-separated data shard DSNs (order matters: index = shard number)")
	flag.StringVar(&lookupDSNs, "lookup-dsns", strings.Join(cfg.Shard.LookupShardDSNs, ","), "comma-separated lookup shard DSNs (order matters: index = shard number)")
	flag.IntVar(&opts.vnodes, "vnodes", cfg.Shard.VnodesPerShard, "vnodes per shard; must match the server's SHARD_VNODES")
	flag.IntVar(&opts.rows, "rows", 10_000, "number of users to insert")
	flag.IntVar(&opts.batch, "batch", 1_000, "users per batch (split across shards, one multi-row INSERT per shard)")
	flag.IntVar(&opts.start, "start", 1, "first user index, for resuming a partial run")
	flag.IntVar(&opts.cost, "bcrypt-cost", cfg.Bcrypt.Cost, "bcrypt cost for the shared hash")
	flag.BoolVar(&opts.truncate, "truncate", false, "TRUNCATE users and username_lookup on every shard before seeding")
	flag.BoolVar(&opts.ignore, "ignore", false, "use INSERT IGNORE so rows that already exist are skipped")
	flag.Parse()

	opts.dataDSNs = splitCSV(dataDSNs)
	opts.lookupDSNs = splitCSV(lookupDSNs)

	if opts.rows <= 0 || opts.batch <= 0 || opts.start <= 0 {
		log.Fatal("seed: --rows, --batch and --start must be positive")
	}
	if len(opts.dataDSNs) == 0 || len(opts.lookupDSNs) == 0 {
		log.Fatal("seed: need at least one data and one lookup shard (DATA_SHARD_DSNS / LOOKUP_SHARD_DSNS or --data-dsns / --lookup-dsns)")
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
	dataDBs, err := openAll(ctx, "data", opts.dataDSNs)
	if err != nil {
		return err
	}
	defer closeAll(dataDBs)
	lookupDBs, err := openAll(ctx, "lookup", opts.lookupDSNs)
	if err != nil {
		return err
	}
	defer closeAll(lookupDBs)

	// Same constructor, same arguments as db.NewShardedHandle.
	dataRing := sharding.NewRing(len(dataDBs), opts.vnodes)
	lookupRing := sharding.NewRing(len(lookupDBs), opts.vnodes)

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
		log.Print("truncating users and username_lookup on all shards")
		for i, db := range dataDBs {
			if _, err := db.ExecContext(ctx, "TRUNCATE TABLE users"); err != nil {
				return fmt.Errorf("truncate data shard %d: %w", i, err)
			}
		}
		for i, db := range lookupDBs {
			if _, err := db.ExecContext(ctx, "TRUNCATE TABLE username_lookup"); err != nil {
				return fmt.Errorf("truncate lookup shard %d: %w", i, err)
			}
		}
	}

	insertVerb := "INSERT INTO"
	if opts.ignore {
		insertVerb = "INSERT IGNORE INTO"
	}

	ids := newIDAllocator(dataRing)
	// Resuming must hand out exactly the IDs the earlier run would have, so
	// replay the allocator up to --start. Hash-only, no I/O: ~1s per 10M.
	for i := 1; i < opts.start; i++ {
		ids.next(i)
	}

	var (
		started    = time.Now()
		inserted   int
		lastLogged int
		dataCount  = make([]int, len(dataDBs))
		lookCount  = make([]int, len(lookupDBs))
	)
	for offset := 0; offset < opts.rows; offset += opts.batch {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("interrupted after %d rows; resume with --start %d --ignore: %w",
				inserted, opts.start+inserted, err)
		}

		n := min(opts.batch, opts.rows-offset)
		first := opts.start + offset

		// Bucket the batch by destination shard.
		dataRows := make([][]userRow, len(dataDBs))
		lookupRows := make([][]userRow, len(lookupDBs))
		for i := first; i < first+n; i++ {
			name := fmt.Sprintf("user%08d", i)
			row := userRow{id: ids.next(i), name: name}
			d := dataRing.ShardFor(row.id)
			l := lookupRing.ShardForString(name)
			dataRows[d] = append(dataRows[d], row)
			lookupRows[l] = append(lookupRows[l], row)
		}

		if err := writeBatch(ctx, insertVerb, dataDBs, lookupDBs, dataRows, lookupRows, hash); err != nil {
			return fmt.Errorf("batch starting at index %d (resume with --start %d --ignore): %w", first, first, err)
		}
		for s, rs := range dataRows {
			dataCount[s] += len(rs)
		}
		for s, rs := range lookupRows {
			lookCount[s] += len(rs)
		}
		inserted += n

		if inserted-lastLogged >= progressEvery || inserted == opts.rows {
			elapsed := time.Since(started)
			log.Printf("seeded %d/%d users in %s (%.0f users/s) data=%v lookup=%v",
				inserted, opts.rows, elapsed.Truncate(time.Millisecond),
				float64(inserted)/elapsed.Seconds(), dataCount, lookCount)
			lastLogged = inserted
		}
	}

	log.Printf("done: %d users in %s", inserted, time.Since(started).Truncate(time.Millisecond))
	return reportCounts(ctx, dataDBs, lookupDBs)
}

type userRow struct {
	id   uint64
	name string
}

// idAllocator hands out user_ids so that data shards fill exactly evenly.
//
// User i is pinned to data shard (i-1) % N. Each shard keeps a cursor into the
// integers 1, 2, 3, ... and returns the next one the ring routes to it. An ID
// routes to exactly one shard, so two cursors can never return the same ID.
//
// Seeded IDs stay below ~rows*1.2, far under both the snowflake range (any ID
// minted after the epoch is >= 1<<22, and real ones today are ~1e17) and the
// 1<<40 floor the DB tests use, so neither can collide with seed data.
type idAllocator struct {
	ring    *sharding.Ring
	cursors []uint64
}

func newIDAllocator(ring *sharding.Ring) *idAllocator {
	return &idAllocator{ring: ring, cursors: make([]uint64, ring.ShardCount())}
}

func (a *idAllocator) next(userIndex int) uint64 {
	shard := (userIndex - 1) % len(a.cursors)
	for {
		a.cursors[shard]++
		if a.ring.ShardFor(a.cursors[shard]) == shard {
			return a.cursors[shard]
		}
	}
}

// writeBatch writes every shard's slice of the batch, one multi-row INSERT per
// shard, all shards in parallel. Shards are independent databases, so a batch
// costs roughly one shard's round trip instead of the sum of all of them.
//
// Lookup and data rows land in separate transactions on separate databases, so
// a crash can leave a batch half-written; resuming that batch with --ignore
// fills in whatever is missing.
func writeBatch(ctx context.Context, insertVerb string, dataDBs, lookupDBs []*sql.DB, dataRows, lookupRows [][]userRow, hash []byte) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	for s, rows := range dataRows {
		if len(rows) == 0 {
			continue
		}
		wg.Go(func() {
			if err := insertUsers(ctx, dataDBs[s], insertVerb, rows, hash); err != nil {
				record(fmt.Errorf("data shard %d: %w", s, err))
			}
		})
	}
	for s, rows := range lookupRows {
		if len(rows) == 0 {
			continue
		}
		wg.Go(func() {
			if err := insertLookups(ctx, lookupDBs[s], insertVerb, rows); err != nil {
				record(fmt.Errorf("lookup shard %d: %w", s, err))
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// insertUsers and insertLookups each write their rows in a single multi-row
// INSERT inside its own transaction.
//
// Two separate wins. Multi-row INSERT collapses n network round trips into one,
// which is what makes 10M feasible at all. Committing per batch rather than
// wrapping the whole run in one transaction keeps the InnoDB undo log bounded:
// a single 10M-row transaction would balloon it and end up slower than the
// naive version, as well as being unresumable.
//
// n*4 placeholders at ~70 bytes of arguments per row. At the default batch of
// 1000 that is well under MySQL's 64MB max_allowed_packet; a much larger
// --batch would need that checked.
func insertUsers(ctx context.Context, db *sql.DB, insertVerb string, rows []userRow, hash []byte) error {
	args := make([]any, 0, len(rows)*4)
	for _, r := range rows {
		args = append(args, r.id, r.name, hash, r.name)
	}
	q := insertVerb + " users (id, username, password_hash, nickname) VALUES " + placeholders(len(rows), "(?,?,?,?)")
	return execTx(ctx, db, q, args)
}

func insertLookups(ctx context.Context, db *sql.DB, insertVerb string, rows []userRow) error {
	args := make([]any, 0, len(rows)*2)
	for _, r := range rows {
		args = append(args, r.name, r.id)
	}
	q := insertVerb + " username_lookup (username, user_id) VALUES " + placeholders(len(rows), "(?,?)")
	return execTx(ctx, db, q, args)
}

func placeholders(n int, tuple string) string {
	var sb strings.Builder
	sb.Grow(n * (len(tuple) + 1))
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(tuple)
	}
	return sb.String()
}

func execTx(ctx context.Context, db *sql.DB, query string, args []any) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// No-op once Commit has succeeded; guarantees no leaked transaction on any
	// early return.
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	return tx.Commit()
}

// reportCounts logs the real row count on every shard, so an uneven or
// partial seed is visible without opening a mysql shell.
func reportCounts(ctx context.Context, dataDBs, lookupDBs []*sql.DB) error {
	for i, db := range dataDBs {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
			return fmt.Errorf("count data shard %d: %w", i, err)
		}
		log.Printf("data shard %d: %d users", i, n)
	}
	for i, db := range lookupDBs {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM username_lookup").Scan(&n); err != nil {
			return fmt.Errorf("count lookup shard %d: %w", i, err)
		}
		log.Printf("lookup shard %d: %d usernames", i, n)
	}
	return nil
}

func openAll(ctx context.Context, kind string, dsns []string) ([]*sql.DB, error) {
	dbs := make([]*sql.DB, 0, len(dsns))
	for i, dsn := range dsns {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			closeAll(dbs)
			return nil, fmt.Errorf("open %s shard %d: %w", kind, i, err)
		}
		// One writer per shard per batch, so a couple of connections is plenty.
		db.SetMaxOpenConns(2)
		db.SetMaxIdleConns(2)
		dbs = append(dbs, db)
		if err := db.PingContext(ctx); err != nil {
			closeAll(dbs)
			return nil, fmt.Errorf("ping %s shard %d: %w", kind, i, err)
		}
	}
	return dbs, nil
}

func closeAll(dbs []*sql.DB) {
	for _, db := range dbs {
		db.Close()
	}
}

func splitCSV(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
