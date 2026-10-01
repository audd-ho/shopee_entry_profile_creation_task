# Load testing

Everything needed to produce the performance numbers the spec asks for:
3000 login requests/second without the cache layer, 6000 with it, from at
least 200 unique users, with no socket or timeout errors.

---

## 0. One-time setup

Both load generators install with the Go toolchain already on this machine —
no package manager, no docker, no sudo:

    go install go.k6.io/k6@latest                     # -> ~/go/bin/k6
    go install github.com/tsenart/vegeta/v12@latest   # -> ~/go/bin/vegeta
    export PATH="$PATH:$HOME/go/bin"

## 1. Check the infrastructure

MySQL and Redis run as **native systemd services** on this machine, not in
docker, despite `deploy/docker-compose.yml` existing. There is no docker
binary here at all.

    systemctl status mysqld redis
    mysql -h 127.0.0.1 -u app -papp userdb -e 'SELECT COUNT(*) FROM users'
    redis-cli ping

Do not test the ports with bash's `/dev/tcp` redirection. The shell here is
zsh, which does not implement it, so it reports every port as closed whether
or not anything is listening. Use `ss -ltn`, `nc -z`, or the real client.

## 2. Build

    go build -o bin/tcp-server  ./cmd/tcp-server
    go build -o bin/http-server ./cmd/http-server
    go build -o bin/seed        ./cmd/seed
    go test ./...

## 3. Seed

    ./bin/seed --truncate --rows 10000000 --batch 5000 --bcrypt-cost 10

| Flag | Default | Meaning |
|---|---|---|
| `--rows` | 10000 | how many users to insert |
| `--batch` | 1000 | rows per multi-row INSERT; 5000 is ~4x fewer round trips |
| `--start` | 1 | first **username** index (`user%08d`), *not* the database id |
| `--bcrypt-cost` | `BCRYPT_COST`, else 10 | cost of the single shared hash |
| `--truncate` | off | TRUNCATE first. Wipes everything, resets `AUTO_INCREMENT` |
| `--ignore` | off | `INSERT IGNORE`, so duplicate usernames are skipped |
| `--dsn` | `MYSQL_DSN` | override the connection string |

10M rows takes about 85 seconds (~119k rows/s). Every seeded account shares
the password `password123`.

### Appending rather than replacing

`--start` is not auto-detected. Find the next free username index with:

```sql
SELECT MAX(CAST(SUBSTRING(username,5) AS UNSIGNED))
FROM users WHERE username REGEXP '^user[0-9]+$';
```

Do **not** use `MAX(id)` — hand-made accounts consume auto-increment ids, so
the two diverge.

If you forget `--start`, every row collides with `uk_username`. Without
`--ignore` the run aborts; *with* `--ignore` it is worse, because the progress
counter counts **attempted** rows, so it will report
`seeded 10000000/10000000` having inserted nothing. Always confirm with
`SELECT COUNT(*)`.

Prefer `--truncate` over appending when changing bcrypt cost. Appending gives
a 20M-row table, which changes index size and buffer-pool pressure, so the
result is no longer comparable with earlier 10M runs — two variables at once.
Re-seeding costs 85 seconds.

### Always flush Redis after seeding

    redis-cli flushdb

The seeder writes straight to MySQL and never talks to the application, so
nothing invalidates the cache. Two things go stale:

1. `user:<username>` still holds the **old hash at the old cost**, so logins
   verify against it for up to 10 minutes — a "cost 6" benchmark that
   quietly measures cost 10.
2. `TRUNCATE` resets `AUTO_INCREMENT`, so cached `profile:<user_id>` keys now
   refer to different users.

Both self-heal on TTL, but within that window the results are wrong and
nothing warns you. This is a testing-only hazard: in production the handlers
are the only writers and they invalidate correctly.

## 4. Run the servers

    ./bin/tcp-server &      # owns MySQL and Redis; start first
    ./bin/http-server &     # dials the tcp-server

| Env var | Default | Notes |
|---|---|---|
| **`CACHE_LAYER`** | `on` | `off` = "without cache layer". **tcp-server only** |
| **`BCRYPT_COST`** | `10` | new signups and the dummy-verify path only (see below) |
| `HTTP_ADDR` | `:8080` | |
| `TCP_LISTEN` | `:9000` | read by both processes |
| `HTTP_TCP_POOL_SIZE` | `8` | muxed connections http -> tcp. Worth sweeping |
| `MYSQL_MAX_OPEN` | `50` | the custom pool |
| `MYSQL_MAX_WAIT_MS` | `200` | pool wait before failing |
| `REDIS_POOL_SIZE` | `50` | saturation here shows up as HTTP 500s |
| `UPLOADS_ROOT` | `uploads` | |
| `SESSION_TTL_SECONDS` | `86400` | |
| `WEB_ROOT` | `web` | |
| `COOKIE_SECURE` | unset | set to `1` only behind TLS |

Confirm the mode from the tcp-server's own log rather than from the variable
you think you set. `CACHE_LAYER=off` prints:

    cache: user/profile cache DISABLED (CACHE_LAYER=off); sessions still cached

Silence means the cache is on.

### `BCRYPT_COST` does not control login speed

bcrypt stores its cost inside the hash (`$2a$10$...`), and verification reads
it from there. So a cost-6 row verifies at cost 6 no matter what the server is
configured with — measured at 4.5ms against a server running `BCRYPT_COST=10`.

`BCRYPT_COST` affects exactly two paths: hashing a new signup, and the dummy
bcrypt burned for an **unknown** username. Set it to match the seeded cost
anyway, for two reasons:

1. Mismatched, a real login takes 4.5ms and a nonexistent one takes 65ms. That
   14x gap is username enumeration, which is the whole reason `DummyVerify`
   exists.
2. Any signup during testing is hashed at the configured cost and lands in a
   table of differently-costed rows, giving later runs an unexplainable tail.

## 5. Run a load test

### The whole experiment, one command

**One invocation runs both halves.** Combine whatever flags you need; these are
not steps to run in sequence.

    ./loadtest/run-experiment.sh --reseed --cost 6 \
        --rate-with-cache 6000 --rate-without-cache 3000 --duration 30s

What that single command does, in order:

1. TRUNCATE and re-seed 10M rows at bcrypt cost 6 *(only because `--reseed`
   was given; without it the existing data is used as-is)*
2. flush Redis, start the servers with `CACHE_LAYER=on` and `BCRYPT_COST=6`,
   load at **6000/s** for 30s, stop them
3. flush Redis, start the servers with `CACHE_LAYER=off`, load at **3000/s**
   for 30s, stop them
4. print a comparison table of both halves

There are two rate flags because the spec sets different targets for the two
halves — 6000/s with the cache layer, 3000/s without — so a single `--rate`
could not express the experiment.

Other useful forms:

    ./loadtest/run-experiment.sh                      # both halves, defaults, no re-seed
    ./loadtest/run-experiment.sh --only without       # skip the with-cache half
    ./loadtest/run-experiment.sh --help               # prints usage, runs nothing

`--only with` / `--only without` exist for iterating on one side without
paying for both. `--rate-on`/`--rate-off` are accepted as shorter aliases for
the two rate flags, and `--only on|off` for `with|without`.

It starts its own servers and kills only those PIDs, so a server you started
by hand is never touched. If its ports are busy it refuses to run rather than
guessing — pass `--http-port`/`--tcp-port` to work alongside a running stack.
`--help` lists every flag.

### A single run

    ./loadtest/run-login.sh 3000 30s
    BASE=http://127.0.0.1:8081 USERS=500 ./loadtest/run-login.sh 3000 30s

Takes the rate and duration; reads `BASE`, `USERS`, `OUT`, `K6`, `LABEL` from
the environment. Snapshots Redis and MySQL counters around the run and prints
the deltas. It does not touch the servers — configure those yourself.

### k6 directly

    k6 run -e RATE=3000 -e DURATION=30s -e USERS=200 loadtest/login.js

`loadtest/login.js` builds each request at runtime, so "200 unique users" is
the `USERS` constant, not 200 files to manage. The users are spread across the
whole 10M keyspace by a stride, so the test cannot accidentally benefit from
rows sharing InnoDB pages.

### vegeta

Needs a body file per user, so generate them once — you never write these by
hand:

    ./loadtest/gen-vegeta-targets.sh 200
    mkdir -p loadtest/results
    R=loadtest/results/vegeta-$(date +%H%M%S).bin
    vegeta attack -targets=loadtest/vegeta/targets.txt -rate=3000 \
      -duration=30s -timeout=15s -max-workers=1000 > "$R"
    vegeta report "$R"
    vegeta report -type=json "$R"      # keys are "50th", "99th"
    vegeta plot "$R" > "${R%.bin}.html"

Write the output under `loadtest/results/`, which is gitignored; dropping a
`result.bin` in the repo root just leaves an untracked binary behind.

| Flag | Default | Meaning |
|---|---|---|
| `-duration` | **`0`, meaning forever** | always set this |
| `-rate` | `50/1s` | arrival rate; `-rate=0` is unbounded |
| `-timeout` | `30s` | per-request timeout |
| `-max-workers` | unlimited | concurrency ceiling |
| `-connections` | `10000` | max idle keep-alive connections per host |

Omitting `-duration` attacks until you interrupt it. k6 is the better default;
vegeta earns its place as a second opinion, being the stricter open-loop
generator. If the two disagree about achieved throughput, believe vegeta.

## 6. Why open-loop matters

Both generators are configured to send at a fixed arrival rate whether or not
the server keeps up. A closed-loop tool (`ab`, `hey`, plain `wrk`) waits for
each response before sending the next, so when the server stalls the generator
stalls with it: it never drives the rate you asked for, and it omits exactly
the slow requests you were looking for. That is coordinated omission, and it
is why "supports 3000 requests per second" cannot be answered with `ab`.

If k6 reports `dropped_iterations` or warns that it could not allocate enough
VUs, the run degraded toward closed-loop and the result is void. Raise
`preAllocatedVUs`/`maxVUs` in `login.js` and rerun.

## 7. Reading the output

- **Cache hit ratio.** During a pure login test the only Redis *reads* are
  `user:<username>` lookups — sessions are written but never read — so the
  `keyspace_hits`/`keyspace_misses` delta measures the user cache and nothing
  else. With `CACHE_LAYER=off` both must be zero; anything else means another
  process is sharing this Redis.
- **`mysql_queries`.** With the cache on this should be a tiny fraction of the
  request count; with it off, roughly one per request.
- **Socket/timeout errors.** `socket_errors` must be 0. An HTTP 504 is the
  http-server's own 5s `backendTimeout`, not a socket error, and appears as a
  failed check instead.
- **`peak_cpu`.** Pinned near 100% means CPU-bound, and on the login path that
  means bcrypt.

## 8. Known ceiling on this hardware

At bcrypt cost 10 a login costs ~65ms of CPU and bcrypt does not overlap, so
14 cores give roughly `14 / 0.065 = 215` logins/second. Measured sustainable
rate is 150-180/s; at 3000/s the service delivers ~15/s with a 0.8% success
rate, 504s from the backend timeout and 500s from the Redis pool timing out.

The cache does not change this. Measured at 150/s, cost 10:

| | cache on | cache off |
|---|---|---|
| MySQL queries | 21 | 2268 |
| Redis hit ratio | 99.6% | n/a (no reads) |
| p95 latency | 68ms | 68ms |

It removes 2,247 database round trips and changes response time by nothing,
because the database lookup is ~1ms of a 65ms request. The spec's
3000-vs-6000 assumption only holds once the hash cost is low enough for the
lookup to matter: at cost 6 a login is ~4.5ms, which puts ~3100/s within
reach of these 14 cores and makes the cache's contribution visible.

## 9. Do not run the generator on this box

k6 or vegeta and the servers share the same 14 cores, so at high rates the
generator competes with the thing it is measuring. A stray `vegeta attack`
left running without `-duration` will silently invalidate every concurrent
measurement. Run the generator from another machine where possible; otherwise
say so in the write-up, and check `ps aux | grep -E 'k6|vegeta'` before
trusting a result.




# SELF RUN TESTS

`redis-cli flushdb` (ensure no leftover from previous run, cache invalidation and wrong/etc passwords etc)
`./bin/seed --truncate --rows 10000000 --batch 5000 --bcrypt-cost X` {X = BCRYPT_COST (4 to 10) [min is 4]}
`CACHE_LAYER=Y BCRYPT_COST=X ./bin/tcp-server & ./bin/http-server` {X = BCRYPT_COST (4 to 10) [min is 4], Y = CACHE_LAYER (on/off)}
`vegeta attack -targets=loadtest/vegeta/targets.txt -rate=3000 -duration=30s -timeout=15s -max-workers=1000 > loadtest/results/{Y}Cache_cost{X}_result.bin` {X = BCRYPT_COST (4 to 10) [min is 4], Y = CACHE_LAYER (on/off)}
`vegeta report loadtest/results/{Y}Cache_cost{X}_result.bin` {X = BCRYPT_COST (4 to 10) [min is 4], Y = CACHE_LAYER (on/off)}


`redis-cli info stats` to see keyspace_hits, keyspace_misses, 


- ### vegeta report loadtest/results/noCache_cost5_result.bin
```
Requests      [total, rate, throughput]         90000, 3000.00, 2999.76
Duration      [total, attack, wait]             30.002s, 30s, 2.422ms
Latencies     [min, mean, 50, 90, 95, 99, max]  1.675ms, 4.766ms, 3.558ms, 6.608ms, 9.009ms, 29.204ms, 191.806ms
Bytes In      [total, mean]                     1788300, 19.87
Bytes Out     [total, mean]                     4680000, 52.00
Success       [ratio]                           100.00%
Status Codes  [code:count]                      200:90000
Error Set:
```
- ### vegeta report loadtest/results/yesCache_cost5_result.bin
```
Requests      [total, rate, throughput]         89999, 3000.31, 3000.08
Duration      [total, attack, wait]             29.999s, 29.997s, 2.299ms
Latencies     [min, mean, 50, 90, 95, 99, max]  1.754ms, 4.268ms, 3.538ms, 6.436ms, 8.348ms, 15.677ms, 83.335ms
Bytes In      [total, mean]                     1788280, 19.87
Bytes Out     [total, mean]                     4679948, 52.00
Success       [ratio]                           100.00%
Status Codes  [code:count]                      200:89999
Error Set:
```
- ### vegeta report loadtest/results/noCache_cost6_result.bin
```
Requests      [total, rate, throughput]         89124, 2967.80, 2635.32
Duration      [total, attack, wait]             30.271s, 30.03s, 240.389ms
Latencies     [min, mean, 50, 90, 95, 99, max]  3.91ms, 320.981ms, 274.889ms, 552.271ms, 684.607ms, 958.439ms, 1.759s
Bytes In      [total, mean]                     1781457, 19.99
Bytes Out     [total, mean]                     4634448, 52.00
Success       [ratio]                           89.51%
Status Codes  [code:count]                      200:79773  500:9351
Error Set:
500 Internal Server Error
```

- ### vegeta report loadtest/results/yesCache_cost6_result.bin
```
Requests      [total, rate, throughput]         88747, 2957.82, 2656.29
Duration      [total, attack, wait]             30.427s, 30.004s, 422.766ms
Latencies     [min, mean, 50, 90, 95, 99, max]  3.65ms, 330.487ms, 283.429ms, 599.876ms, 722.479ms, 1.031s, 2.091s
Bytes In      [total, mean]                     1772389, 19.97
Bytes Out     [total, mean]                     4614844, 52.00
Success       [ratio]                           91.07%
Status Codes  [code:count]                      200:80823  500:7924
Error Set:
500 Internal Server Error
```





### v2

- ### vegeta report loadtest/results/noCache_cost5_result_v2.bin
```
vegeta report loadtest/results/noCache_cost5_result_v2.bin
Requests      [total, rate, throughput]         90001, 2999.99, 2999.70
Duration      [total, attack, wait]             30.003s, 30s, 2.871ms
Latencies     [min, mean, 50, 90, 95, 99, max]  1.8ms, 10.183ms, 4.728ms, 12.896ms, 31.625ms, 147.591ms, 353.49ms
Bytes In      [total, mean]                     1788314, 19.87
Bytes Out     [total, mean]                     4680052, 52.00
Success       [ratio]                           100.00%
Status Codes  [code:count]                      200:90001  
Error Set:
```

- ### vegeta report loadtest/results/yesCache_cost5_result_v2.bin
```
vegeta report loadtest/results/yesCache_cost5_result_v2.bin
Requests      [total, rate, throughput]         89999, 2999.94, 2999.69
Duration      [total, attack, wait]             30.003s, 30s, 2.523ms
Latencies     [min, mean, 50, 90, 95, 99, max]  1.709ms, 6.802ms, 4.297ms, 9.217ms, 15.781ms, 71.092ms, 198.751ms
Bytes In      [total, mean]                     1788280, 19.87
Bytes Out     [total, mean]                     4679948, 52.00
Success       [ratio]                           100.00%
Status Codes  [code:count]                      200:89999  
Error Set:
```

latency drop, loook at p90, p95, p99 and max (biggest impact)

REDIS:

keyspace_hits:421869
keyspace_misses:2539

to

keyspace_hits:511666
keyspace_misses:2741

change is 89797, 202

so

keyspace_hits:89797
keyspace_misses:202
total: 89999 (matches up)