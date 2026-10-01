// Measures single-core bcrypt cost and extrapolates theoretical max throughput.
//
// Usage:
//   go run ./cmd/bcrypt-bench -cost 5 -n 30
//
// Controlling cores:
//   GOMAXPROCS only affects the process it's set on. Setting it here changes
//   what this bench reports, but does NOT affect your running tcp-server or
//   http-server — those are separate processes with their own GOMAXPROCS
//   (defaults to runtime.NumCPU()).
//
//   GOMAXPROCS=4 go run ./cmd/bcrypt-bench -cost 5    # "what if I had 4 cores"
//   GOMAXPROCS=10 ./bin/tcp-server                    # actually limit the server
//   taskset -c 0-3 ./bin/tcp-server                   # OS-level core pinning
//
// The numbers here are useful for quick "what if" math (e.g. how many cores
// does bcrypt alone need to sustain 3k RPS at cost 5?). The real answer comes
// from vegeta/wrk against the actual server, where DB, Redis, GC, and the
// TCP protocol all compete for those same cores.
package main

import (
	"flag"
	"fmt"
	"runtime"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	cost := flag.Int("cost", 5, "bcrypt cost factor")
	rounds := flag.Int("n", 20, "number of hashes to average over")
	flag.Parse()

	password := []byte("password123")
	hash, err := bcrypt.GenerateFromPassword(password, *cost)
	if err != nil {
		fmt.Printf("generate failed: %v\n", err)
		return
	}

	// Warm up
	bcrypt.CompareHashAndPassword(hash, password)

	// Benchmark hash generation
	start := time.Now()
	for range *rounds {
		bcrypt.GenerateFromPassword(password, *cost)
	}
	genTotal := time.Since(start)
	genPer := genTotal / time.Duration(*rounds)

	// Benchmark comparison (what login actually does)
	start = time.Now()
	for range *rounds {
		bcrypt.CompareHashAndPassword(hash, password)
	}
	cmpTotal := time.Since(start)
	cmpPer := cmpTotal / time.Duration(*rounds)

	cores := runtime.NumCPU()

	fmt.Printf("bcrypt cost %d, averaged over %d iterations\n", *cost, *rounds)
	fmt.Printf("  Generate:  %s/op\n", genPer)
	fmt.Printf("  Compare:   %s/op  (this is what login does)\n", cmpPer)
	fmt.Printf("\n")
	fmt.Printf("Cores available: %d\n", cores)
	fmt.Printf("Theoretical max (compare, all cores saturated):\n")
	fmt.Printf("  %d logins/sec  (cache-miss, every login pays bcrypt)\n",
		int(float64(cores)/cmpPer.Seconds()))
	fmt.Printf("  With 200 unique users cached at 10min TTL:\n")
	fmt.Printf("    Cold logins in a 10min window: 200\n")
	fmt.Printf("    Bcrypt wall time for cold set: %s (serialized on 1 core)\n",
		cmpPer*time.Duration(200))
	fmt.Printf("    Bcrypt wall time parallelized: %s (across %d cores)\n",
		cmpPer*time.Duration(200)/time.Duration(cores), cores)
}
