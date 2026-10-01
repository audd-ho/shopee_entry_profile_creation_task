# Sharding, Scaling, and Rate Limiting — Reference & Project Design

This document has two halves. The **top half** (§1–§7) is a general reference:
sharding methods, consistent hashing, ID generation, migration strategies,
replication/availability, load balancing/gateway design, and rate limiting
algorithms — all framework-agnostic, applicable beyond this project. The
**bottom half** (§8+) zooms into this project's specific decisions and
implementation — to be filled in after the general concepts are discussed and
settled.

---

# Part I — General Concepts & Reference

---

## 1. Sharding — Why and When

### 1.1 What Sharding Is

Sharding (horizontal partitioning) splits a single logical dataset across
multiple physical database instances, each holding a subset of the rows. Every
shard has the same schema; the data is divided by a **shard key**.

Sharding is not the same as:
- **Replication** — copies of the same data for availability/read scaling.
- **Vertical partitioning** — splitting columns into separate tables.
- **Federation** — splitting by function (users DB, orders DB, etc.).

### 1.2 When to Shard

Shard when a single instance can no longer handle the load along one or more of:
- **Write throughput** — disk IOPS, redo log, lock contention.
- **Storage** — dataset exceeds what one machine's disk/buffer pool can hold.
- **Read throughput** — read replicas help, but eventually the primary's
  replication fan-out is the bottleneck.

Do NOT shard prematurely. A single well-tuned MySQL instance can handle millions
of rows and thousands of QPS. Sharding adds operational complexity (routing,
migration, cross-shard queries, distributed transactions). The right time is
when vertical scaling (bigger machine) and read replicas are exhausted.

### 1.3 What Sharding Does NOT Solve

- **Availability.** A shard going down takes its subset of users offline.
  Sharding increases blast radius per-instance (fraction of users down vs
  degraded performance for all). Availability is solved by **replication** (§5).
- **Query patterns that span shards.** A `SELECT ... WHERE username LIKE '%alice%'`
  across 10 shards requires fan-out to all 10 — slower than one big table.
- **Transactions across shards.** Two rows on different shards cannot share a
  MySQL transaction. Need application-level saga/compensation or a distributed
  transaction coordinator.

---

## 2. Sharding Methods

### 2.1 Hash-Based (Key-Based)

`shard = hash(shard_key) % N` or consistent hash (§3).

| Pros | Cons |
|---|---|
| Even distribution if hash is uniform | Range queries require fan-out to all shards |
| No hot spots (assuming uniform key distribution) | Adding/removing shards reshuffles keys (mitigated by consistent hashing) |
| Simple routing — pure function of the key | No locality — related rows scatter across shards |

Best for: point lookups by key, write-heavy workloads, hot-spot-averse systems.
Examples: Redis Cluster (16384 hash slots), DynamoDB, Cassandra.

### 2.2 Range-Based

`shard = which range does shard_key fall into?` (e.g. users 1–1M → shard 0,
1M–2M → shard 1, etc.)

| Pros | Cons |
|---|---|
| Range queries are efficient (scan one shard) | Hot spots — new data (recent IDs, recent timestamps) clusters on one shard |
| Related rows co-locate (locality) | Uneven load distribution over time |
| Adding shards = splitting a range (no full reshuffle) | Range boundaries need manual management or auto-splitting |

Best for: time-series data, sequential access patterns, single-machine or when
locality matters. Examples: HBase, Spanner (with auto-splitting), CockroachDB.

### 2.3 Directory-Based (Lookup Table)

A separate service/table maps each key (or key range) to a shard.

| Pros | Cons |
|---|---|
| Maximum flexibility — any key can live on any shard | Extra hop on every request (the lookup) |
| Resharding = updating the directory, not moving all keys | The directory is a single point of failure / bottleneck |
| Supports arbitrary, non-uniform placement | More complex operationally |

Best for: multi-tenant systems where tenants vary wildly in size, or hybrid
strategies where some keys are pinned for compliance/locality reasons.

### 2.4 Hybrid Patterns

Real systems combine these:
- **Hash across machines, range within each machine.** Cross-machine load
  balanced; within-machine cohort locality preserved. Best of both.
- **Consistent hash for routing + directory for overrides.** Most keys follow
  the hash; a few hot/large tenants are pinned to dedicated shards via the
  directory.
- **Two-level sharding.** First level: hash to a coarse bucket (1024 virtual
  shards). Second level: map virtual shard → physical machine. Adding a machine
  = reassigning some virtual shards, not rehashing all keys.

---

## 3. Consistent Hashing

### 3.1 The Problem with Modulo

`shard = key % N`. Changing N (adding a shard) reshuffles ~(N-1)/N of all keys.
With 3→4 shards, ~75% of keys move. This means migrating 75% of your data to
new locations — a massive, slow, risky operation.

### 3.2 The Ring

Nodes (shards) and keys are hashed onto a circular number line (typically
0 to 2³²-1). A key hashes to a position on the ring and walks **clockwise** to
the first node it encounters. That node owns it.

```
        Node A (pos 100)
       /
  0 ──●────────────●── Node B (pos 400)
  │      Ring           │
  │   (0 to 2³²-1)     │
  └────●────────────────┘
     Node C (pos 700)

Key X hashes to 250 → clockwise → Node B
Key Y hashes to 500 → clockwise → Node C
Key Z hashes to 50  → clockwise → Node A
```

### 3.3 Adding a Node

Node D joins at position 550:
```
Before:  keys 401-700 all owned by Node C
After:   keys 401-550 → Node D  (taken FROM Node C only)
         keys 551-700 → Node C  (unchanged)
         All other arcs: unchanged
```

Only keys between the new node and its predecessor (Node B at 400) move. They
come from **one neighbor** (Node C). ~1/N of keys move, not all of them.

### 3.4 Removing a Node

Node D leaves → its keys (401-550) walk clockwise to the next node (Node C).
One neighbor absorbs them. Still ~1/N movement.

### 3.5 Virtual Nodes (Vnodes)

With few physical nodes, the arc lengths between them are uneven by chance — one
node might own 60% of the ring. Fix: each physical node gets many positions
(100-200 **virtual nodes**) on the ring.

```
Physical Node A: vnodes at positions 100, 1500, 3200, 4800, 6100, ...
Physical Node B: vnodes at positions 400, 1800, 3500, 5100, 6500, ...
Physical Node C: vnodes at positions 700, 2100, 3800, 5400, 6900, ...
```

A key hashes to 1600 → walks clockwise → hits vnode at 1800 → physical Node B.

Benefits:
- **Even distribution.** With 150 vnodes per node, the statistical variation in
  arc sizes is small — each node owns ~1/N of the ring within a few percent.
- **Smooth rebalancing.** Adding a physical node distributes its vnodes around
  the ring. Each vnode steals a small arc from a different neighbor, so the load
  removed from each existing node is roughly equal.
- **Heterogeneous hardware.** A more powerful machine gets more vnodes → owns
  more of the ring → handles a proportionally larger share of traffic.

### 3.6 Implementation

~50 lines in any language: a sorted array of `(hash_value, shard_id)` pairs.
For each key, hash it, binary-search the array for the first entry ≥ the hash
(wrapping around). O(log V) where V = total vnodes — negligible.

Libraries: `hashicorp/consistent` (Go), `ketama` (C), many others. Or hand-roll
for full understanding.

### 3.7 Consistent Hashing vs. Other Approaches

| Approach | Keys moved on +1 node | Routing complexity | Balance |
|---|---|---|---|
| `key % N` | ~(N-1)/N (catastrophic) | O(1) | Perfect |
| Consistent hash (no vnodes) | ~1/N | O(log N) | Poor (uneven arcs) |
| Consistent hash (with vnodes) | ~1/N | O(log V) | Good (even arcs) |
| Jump consistent hash | ~1/N | O(1) | Perfect | Only supports adding/removing the last node |
| Rendezvous (HRW) hashing | ~1/N | O(N) per lookup | Perfect |

For most systems, consistent hashing with vnodes is the sweet spot —
good balance, efficient lookup, supports arbitrary add/remove.

---

## 4. ID Generation in a Sharded System

### 4.1 The Problem

`AUTO_INCREMENT` generates IDs on one MySQL instance. With multiple shards, two
instances independently increment and produce colliding IDs.

### 4.2 Options

#### (A) Central Auto-Increment Counter

A single service/DB that hands out IDs. Every signup asks it for the next number.

| Pros | Cons |
|---|---|
| Sequential, dense, simple | Single point of failure |
| Easy to reason about | Bottleneck under high write throughput |
| | Network round trip per ID |

Variants: Flickr's ticket server approach — two MySQL instances, one with
`auto_increment_offset=1, increment=2` (odd), the other offset=2 (even). Either
can fail without collision. Generalises to N servers with offset=N.

#### (B) DB Increment-by-Step

Each shard uses `auto_increment_increment=N, auto_increment_offset=shard_id`.
Shard 0 produces 1, 4, 7, 10, ...; Shard 1 produces 2, 5, 8, 11, ...

| Pros | Cons |
|---|---|
| No central service | Coupled to shard count — changing N breaks the scheme |
| No coordination at runtime | IDs reveal shard membership |
| Sequential within a shard | Adding a shard requires changing increment on all existing shards |

#### (C) UUID (128-bit)

Randomly generated, no coordination.

| Pros | Cons |
|---|---|
| Zero coordination | 128 bits — doesn't fit BIGINT (needs BINARY(16) or CHAR(36)) |
| Globally unique by construction | 2× primary key width → larger indexes, more memory |
| | **Random scatter** — inserts land all over the B-tree → page splits, poor buffer pool hit rate |
| | Not human-readable or sortable |

UUIDv7 (draft RFC) addresses the sort order issue by embedding a Unix timestamp
in the high bits, but is still 128 bits.

#### (D) ULID (128-bit, time-sorted)

Universally Unique Lexicographically Sortable Identifier. 48-bit timestamp +
80-bit random.

| Pros | Cons |
|---|---|
| Time-sorted (good B-tree locality) | Still 128 bits |
| Lexicographically sortable as a string | Less widely supported than UUID |
| Monotonic option within the same ms | |

#### (E) Snowflake (64-bit, time-sorted) — industry standard

Each process generates IDs independently using bit-packed components.

```
 64-bit layout:
 ┌─────────────────────────────┬──────────┬──────────────┐
 │  41 bits: millisecond       │ 10 bits: │ 12 bits:     │
 │  timestamp since custom     │ machine  │ sequence     │
 │  epoch (~69.7 years)        │ ID (1024)│ (4096/ms)    │
 └─────────────────────────────┴──────────┴──────────────┘
```

| Pros | Cons |
|---|---|
| 64-bit — fits BIGINT, same PK width as AUTO_INCREMENT | Needs machine ID coordination (one-time, at startup) |
| Time-sorted — excellent B-tree locality | Clock skew can cause non-monotonic IDs across machines (ms-level, typically harmless) |
| Zero runtime coordination | IDs are not dense (gaps from machine/sequence bits) |
| ~4M IDs/sec/machine | Epoch has a 69-year lifespan (pick a recent custom epoch) |

**Density doesn't matter.** Nobody queries `WHERE id BETWEEN 1 AND 1000`
expecting exactly 1000 rows. AUTO_INCREMENT has gaps too (rolled-back
transactions, deletes). The B-tree only cares about order and fixed width —
`5 < 6` costs the same CPU as `483927481 < 892034712` (one 64-bit `CMP`
instruction, one register, one cycle).

#### (F) Comparison Table

| Approach | Bits | Fits BIGINT | B-tree locality | Coordination | Survives resharding |
|---|---|---|---|---|---|
| Central counter | 64 | ✅ | ✅ sequential | Per-ID (bottleneck) | ✅ |
| DB step increment | 64 | ✅ | ✅ sequential | Config-time only | ❌ (N change) |
| UUID v4 | 128 | ❌ | ❌ random scatter | None | ✅ |
| UUID v7 / ULID | 128 | ❌ | ✅ time-sorted | None | ✅ |
| **Snowflake** | **64** | **✅** | **✅ time-sorted** | **Machine ID only** | **✅** |

### 4.3 Snowflake — Deep Dive

**Custom epoch:** use the service launch date rather than Unix 1970. This avoids
wasting 41 bits of timestamp space on the ~56 years that already passed. 2⁴¹ ms
from a 2026 epoch lasts until ~2095.

**Machine ID assignment:** the only coordination, and it's one-time at startup.
Options:
- **Config file / env var:** simplest. Each instance is configured with a unique
  `MACHINE_ID=0..1023`. Works well for a small, stable fleet.
- **Coordination service (ZooKeeper, etcd):** leases a machine ID on startup,
  releases on shutdown. Handles dynamic scaling — a new instance gets the next
  free ID automatically.
- **IP/hostname hash:** `hash(IP) % 1024`. No coordination needed, but collision
  risk if the hash function isn't perfect. Mitigated by the sequence bits
  (collisions only matter if two machines get the same machine ID AND generate
  an ID in the same millisecond AND at the same sequence number).

**Sequence overflow:** if a machine exhausts 4096 sequence numbers in one
millisecond, it busy-waits until the next millisecond. At 4,096,000 IDs/sec this
is far beyond any realistic signup rate.

**Clock skew:** if the system clock jumps backward (NTP correction, VM migration),
the generator could produce IDs with a timestamp in the past, potentially
colliding with IDs it already generated. Mitigations:
- Refuse to generate IDs if `now < lastTimestamp` (fail-fast, operator
  investigates).
- Use `lastTimestamp` instead of `now` until the clock catches up (generates
  IDs with a slightly-future timestamp from the perspective of the jumped clock).
- NTP slew mode (adjusts gradually, never jumps backward) prevents the issue
  entirely.

**Implementation in Go vs C++:**

Go has no bitfield syntax. Shifts are explicit:
```go
id := (timestamp << 22) | (machineID << 12) | sequence

timestamp := id >> 22
machineID := (id >> 12) & 0x3FF
sequence  := id & 0xFFF
```

C++ has bitfields:
```cpp
struct SnowflakeID {
    uint64_t sequence   : 12;
    uint64_t machine_id : 10;
    uint64_t timestamp  : 41;
    uint64_t unused     : 1;
};
```

But C++ bitfield **bit ordering is implementation-defined** — varies by compiler,
platform, and endianness. A bitfield struct cannot be portably serialized to disk
or sent over the network and read back on a different platform. For this reason,
most production C++ snowflake implementations also use explicit shifts, same as
Go. The bitfield syntax is syntactic sugar for in-process use only.

---

## 5. Replication and Availability

### 5.1 Sharding ≠ Availability

Sharding distributes load. Replication provides availability. They compose:

```
┌── Shard 0: Primary ←→ Replica(s)
├── Shard 1: Primary ←→ Replica(s)
└── Shard 2: Primary ←→ Replica(s)
```

Each shard has its own replication group. A primary failure triggers promotion
of a replica. The sharding layer is above this — it routes to the shard, and
the shard's own replication handles failover.

### 5.2 MySQL Replication Modes

| Mode | How it works | Data loss on failover | Read-from-replica staleness |
|---|---|---|---|
| **Async** (default) | Primary commits, ships binlog asynchronously. Replica applies later. | Possible (unreplicated writes lost) | Unbounded (seconds of lag) |
| **Semi-sync** | Primary waits for ≥1 replica to ACK receipt of binlog before returning OK. | None (≥1 replica has the binlog) | Still present (ACK = received, not yet applied) |
| **Group Replication** (Paxos) | Leader proposes → majority ACK → commit. | None (majority consensus) | Still present on secondaries (log replay lag) |

### 5.3 The Staleness Problem

**Replication guarantees consistency of the log, not zero-staleness of reads
from followers.** Raft/Paxos ensure every node agrees on the ORDER of
operations, but a follower that hasn't finished replaying the latest committed
entry still serves stale data.

- **Read from primary:** always fresh. But defeats the point of read replicas.
- **Read from follower:** potentially stale by the replay lag (ms to seconds).

### 5.4 "Read Your Own Writes" Pattern

The practical middle ground. After a write, the user who wrote should see their
own change immediately. Everyone else can tolerate brief staleness.

```
1. User updates nickname → write to primary, get GTID (global transaction ID)
2. User's next read → pass GTID to replica:
   WAIT_FOR_EXECUTED_GTID_SET(gtid, timeout_ms)
3. Replica catches up (typically ms) → serve from replica
   OR timeout → fall back to primary
```

Implementation: carry the GTID in the session or as an HTTP response header.
The next request sends it back, and the read-routing layer uses it to decide
whether the replica is fresh enough.

### 5.5 Connection Pool Behavior on Failover

When a primary fails and a replica promotes, the pool sees:
- Connections to the old primary return `ErrBadConn` or EOF.
- The retry logic (already implemented in pool.go's `isBadConn` check) discards
  the broken connection and dials a new one.
- If the DNS/VIP has flipped to the new primary, the new dial connects to the
  promoted replica.
- Callers experience a brief spike in latency (one failed attempt + one retry),
  not an outage.

No code change needed for basic failover — the existing pool retry mechanism
handles it. What's needed is the infrastructure side: DNS/VIP management, a
replication orchestrator (ProxySQL, MySQL Router, Vitess, Orchestrator).

---

## 6. Load Balancing, Gateways, and Routing

### 6.1 Dumb LB (Layer 4 / Layer 7 Round-Robin)

The simplest approach: a load balancer (HAProxy, nginx, cloud LB) distributes
incoming requests across backend instances without inspecting the request body
or understanding the application protocol.

| Pros | Cons |
|---|---|
| Operationally simple | Cannot route by content (user, shard, etc.) |
| Low latency (no parsing) | All backends must be able to handle all requests |
| Well-understood, battle-tested | Rate limiting by user/key requires the backend to do it |

This is what you want for **stateless backends** — any instance handles any
request. The sharding decision happens inside the backend, not at the LB.

### 6.2 Application-Aware Gateway (Layer 7 with Request Inspection)

A smarter proxy that understands the protocol: parses request bodies, resolves
sessions, computes shard routing, and sends each request to the correct backend.

| Pros | Cons |
|---|---|
| Backends can be specialized (1:1 with shards) | Must understand the application protocol |
| Can do per-user rate limiting at the edge | Single point of failure / bottleneck (the gateway itself) |
| Can reject invalid requests before they reach the backend | Higher latency (parse + lookup + route) |
| | More complex to build, test, and operate |
| | A bug in the gateway takes down all traffic |

### 6.3 When to Use Which

| Scenario | Best fit |
|---|---|
| Stateless backends, all can handle all requests | Dumb LB |
| Need per-user routing before the backend | Application-aware gateway |
| Need rate limiting at the edge (by IP, by user) | Either — but dumb LB + backend rate limiting is simpler |
| Need to shield backends from invalid traffic | Application-aware gateway or a simple validation proxy |

### 6.4 The Gateway-as-Routing-Proxy Problem

If the gateway's job is to route requests to the right shard-specific backend,
it needs to:
1. Parse the request to extract the routing key (username for login, session
   token for post-login)
2. Resolve the routing key (lookup username→user_id, or resolve session→user_id)
3. Compute `ShardFor(user_id)`
4. Route to the backend that owns that shard

This means the gateway needs access to the username lookup AND Redis (for
session resolution). It becomes a critical, stateful component. If it goes down,
all traffic stops. If it has a bug in its routing logic, requests go to the
wrong shard.

**Alternative (usually better):** keep the LB dumb, keep the backends stateless
but shard-aware. Each backend connects to all shards and does the routing
internally. The LB's only job is "pick a healthy backend." This is Option A from
the scaling design — operationally simpler, easier to reason about, and the
failure mode of "one backend dies" is handled by the LB removing it from the
pool, not by a single gateway becoming a bottleneck.

---

## 7. Rate Limiting

### 7.1 Why Rate Limit

- **Brute-force protection:** an attacker trying millions of passwords per second.
- **DoS mitigation:** a flood of requests overwhelming the backend.
- **Fair use:** one user consuming disproportionate resources.
- **Cost control:** expensive operations (bcrypt, DB writes) bounded per-client.

### 7.2 Where to Rate Limit

| Layer | What it sees | Good for | Bad for |
|---|---|---|---|
| **Edge / LB** | IP address, request rate | IP-based flood protection, connection limits | Per-user limits (doesn't know who the user is) |
| **Application-aware gateway** | IP, user ID (after session resolve), route | Per-user limits, per-route limits | Adds latency, complexity; is a bottleneck |
| **Backend (TCP server)** | Everything — session, user ID, route, payload | Full context for fine-grained limiting | Late in the pipeline — request already consumed resources getting here |

**Best practice:** defense in depth — coarse at the edge (IP-based), fine-grained
at the backend (per-user, per-route).

### 7.3 Rate Limiting Algorithms

#### (A) Fixed Window

Divide time into fixed windows (e.g. 1-minute buckets). Count requests per
window. Reject if count > threshold.

```
Window: 12:00-12:01  → counter: 47
Window: 12:01-12:02  → counter: 0 (reset)
```

Implementation: `INCR key; EXPIRE key window_seconds` in Redis. One round trip.

| Pros | Cons |
|---|---|
| Simplest to implement | **Boundary burst:** 100 requests at 12:00:59 + 100 at 12:01:00 = 200 in 2 seconds, despite a 100/min limit |
| O(1) per request | Window alignment is arbitrary |
| Low Redis overhead | |

#### (B) Sliding Window Log

Store the timestamp of every request. Count timestamps within the window.

```
Requests: [12:00:01, 12:00:15, 12:00:42, 12:01:03]
Window = last 60s from now (12:01:10) → count requests after 12:00:10 → 3
```

Implementation: Redis sorted set (ZADD timestamp, ZREMRANGEBYSCORE to prune,
ZCARD to count).

| Pros | Cons |
|---|---|
| Precise — no boundary burst problem | O(N) memory per client (stores every timestamp) |
| Smooth limiting | More Redis ops per check (ZADD + ZREMRANGEBYSCORE + ZCARD) |

#### (C) Sliding Window Counter (Hybrid)

Approximate the sliding window using two fixed-window counters and a weighted
average.

```
Previous window (12:00-12:01): 80 requests
Current window  (12:01-12:02): 30 requests (so far)
Time into current window: 40% (24 seconds out of 60)

Approximate sliding count = 80 × (1 - 0.4) + 30 = 78
```

| Pros | Cons |
|---|---|
| Smooths the boundary burst problem | Approximate (not exact) |
| O(1) memory per client (two counters) | Slightly more complex than fixed window |
| Low Redis overhead | |

**This is what most production systems use** — good enough accuracy, minimal
overhead.

#### (D) Token Bucket

A bucket holds up to N tokens. Tokens are added at a fixed rate (e.g. 10/sec).
Each request consumes one token. If the bucket is empty, reject.

```
Bucket capacity: 100
Refill rate: 10/sec

t=0:   100 tokens.  Burst of 50 → 50 tokens remain.
t=1:   50 + 10 = 60 tokens.
t=5:   60 + 40 = 100 tokens (capped at capacity).
t=5.1: Request → 99 tokens. Allowed.
```

| Pros | Cons |
|---|---|
| Allows **controlled bursts** up to bucket capacity | More state per client (token count, last refill timestamp) |
| Smooth long-term rate | Implementation slightly more complex |
| Two tuning knobs (rate + burst) | |

Implementation: Redis with a Lua script that atomically computes elapsed time,
adds tokens, and decrements. Or in-memory on the backend.

#### (E) Leaky Bucket

Requests enter a queue (the bucket). The queue drains at a fixed rate. If the
queue is full, reject.

```
Queue capacity: 10
Drain rate: 5/sec

Burst of 15 requests → 10 queued, 5 rejected immediately.
Over the next 2 seconds: 10 drained at 5/sec.
```

| Pros | Cons |
|---|---|
| Produces a perfectly smooth output rate | **Adds latency** — requests sit in a queue waiting their turn |
| No bursts at all (may or may not be desired) | Punishes legitimate burst patterns (page load = 5 simultaneous requests) |
| Simple to reason about | Queue memory proportional to capacity |

**The key difference between token bucket and leaky bucket:**
- Token bucket: **allows bursts** up to the bucket capacity, then throttles.
  Good for bursty-but-bounded workloads (web page loads, API batches).
- Leaky bucket: **smooths all traffic** to a constant rate. Good when the
  downstream can only handle a fixed rate (a serial port, a single-threaded
  worker).

For login rate limiting, **token bucket or sliding window counter** are the
best fits. Login is bursty (a user types their password and submits) and should
be allowed short bursts, but bounded over time to prevent brute-force.

#### (F) Comparison Table

| Algorithm | Burst handling | Memory per client | Redis ops/check | Accuracy |
|---|---|---|---|---|
| Fixed window | Boundary burst problem | O(1) | 1-2 | Coarse |
| Sliding log | No bursts leak through | O(N) | 3 | Exact |
| Sliding counter | Mild boundary leak | O(1) | 2-3 | Approximate |
| Token bucket | Controlled bursts allowed | O(1) | 1 (Lua script) | Exact |
| Leaky bucket | No bursts (adds latency) | O(capacity) | 1 | Exact |

### 7.4 Rate Limiting by Key

Different keys protect against different threats:

| Key | What it protects against | Where to enforce |
|---|---|---|
| **IP address** | Distributed brute-force, DoS floods | Edge / LB or backend |
| **Username** (on login) | Targeted account brute-force | Backend (knows the username after parsing) |
| **Session / User ID** (on authenticated routes) | Abusive authenticated users, API scraping | Backend (knows the user after session resolve) |
| **Global** (total RPS across all clients) | Backend overload protection | Edge / LB |

**Layering:** apply multiple limits simultaneously:
```
Per-IP:       100 requests/min    (catches floods from one source)
Per-username: 5 failed logins/15min (catches targeted brute-force)
Global:       10k RPS total       (protects the backend from aggregate overload)
```

A request is rejected if ANY limit is exceeded.

### 7.5 Rate Limiting in a Distributed System

With multiple backend instances, counters must be shared. Options:

- **Redis (centralized counter):** all instances increment the same key. Exact,
  but adds a Redis round trip per request. At high RPS, pipeline the rate-limit
  check with other Redis ops (e.g. session GET).
- **Local counters (approximate):** each instance keeps its own counter. No
  coordination, but the effective global limit is `per-instance-limit × N
  instances`. Acceptable for coarse protection, not for tight per-user limits.
- **Hybrid:** local counter for coarse rejection (reject obviously over-limit
  requests without a Redis call), Redis for precise limits on borderline cases.

### 7.6 Response to Rate-Limited Requests

- HTTP 429 Too Many Requests (not 403 — 403 means forbidden, not throttled).
- `Retry-After` header with seconds until the limit resets.
- Clear error message: "rate limit exceeded" (not "forbidden" or "unauthorized").
- **Do NOT reveal the limit parameters** in the response (attackers calibrate
  to stay just under the limit).
- For login specifically: rate-limited responses should still take approximately
  the same time as a real login attempt (or an attacker can distinguish
  "rate-limited" from "wrong password" by response time).

---

# Part II — Project-Specific Design

*(To be filled in after discussion of the general concepts above and how they
apply to this project's specific requirements, architecture, and constraints.)*

---

## 8. Sharding Design for This Project

### 8.1 Decisions

| Aspect | Decision | Reasoning |
|---|---|---|
| Shard key | `user_id` | 99% of queries are post-login by ID. Login (by username) is already the slow path (bcrypt dominates); one extra lookup is negligible. |
| Shard method | Consistent hashing with vnodes (§3) | Adding a shard moves ~1/N of keys, not all. Vnodes ensure even distribution. |
| TCP server routing | Dumb round-robin LB to TCP servers | TCP servers are stateless — any handles any user. No application-aware gateway needed. |
| TCP→DB topology | Option A: every TCP server connects to every shard | Decoupled scaling (add TCP for CPU, add shards for IO independently). Free failover — one TCP server dies, others serve all shards. |
| Username lookup | MySQL table on a **separate dedicated instance** + Redis cache on top | Separate instance avoids asymmetric shard load (§8.3). Table survives Redis restarts without a rebuild. Cache-aside on top for performance — same pattern as `auth:<username>`. Scales independently by `hash(username)` if needed. |

### 8.2 Topology

```
   Browser
     │  HTTPS/HTTP
     ▼
┌──────────────┐
│  HTTP Server │  Round-robin LB — no shard awareness
└──────┬───────┘
       │  TCP (multiplexed, round-robin across TCP servers)
       ▼
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│ TCP Server 0 │   │ TCP Server 1 │   │ TCP Server 2 │
│ machine_id=0 │   │ machine_id=1 │   │ machine_id=2 │
│              │   │              │   │              │
│ pools:       │   │ pools:       │   │ pools:       │
│  shard_0_pool│   │  shard_0_pool│   │  shard_0_pool│
│  shard_1_pool│   │  shard_1_pool│   │  shard_1_pool│
│  shard_2_pool│   │  shard_2_pool│   │  shard_2_pool│
│  lookup_pool │   │  lookup_pool │   │  lookup_pool │
└──────────────┘   └──────────────┘   └──────────────┘
       │                  │                  │
       ▼                  ▼                  ▼
  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────────────┐  ┌────────┐
  │ Shard 0 │  │ Shard 1 │  │ Shard 2 │  │ username_lookup │  │ Redis  │
  │  users  │  │  users  │  │  users  │  │ (separate       │  │(shared)│
  │         │  │         │  │         │  │  dedicated       │  │        │
  │(user_id)│  │(user_id)│  │(user_id)│  │  instance)      │  │        │
  └─────────┘  └─────────┘  └─────────┘  └─────────────────┘  └────────┘
```

Each TCP server holds:
- One custom MySQL pool per data shard (same `db.Pool` implementation as today,
  just multiple instances).
- One pool to the `username_lookup` instance (**separate, dedicated MySQL
  instance** — not co-located on any data shard). This avoids asymmetric load:
  putting the lookup on shard 0 would make that shard carry all login lookups
  (100% of login traffic) on top of its own 1/N share of user data queries.
  A separate instance keeps every data shard equally loaded and isolates lookup
  availability from data shard availability — post-login operations keep working
  even if the lookup instance has a problem.
- One Redis client (shared, same as today — sessions + caches).

Shard selection is a one-line call inside the TCP server:
```go
pool := s.shards[ShardFor(userID)]
```

No TCP-to-TCP forwarding. No application-aware LB. The HTTP server's
`tcpclient.go` round-robin is unchanged.

### 8.3 Username Lookup Table

**Source of truth for global username uniqueness.** Separate from the per-shard
`users` tables.

```sql
CREATE TABLE username_lookup (
  username  VARCHAR(32)      NOT NULL,
  user_id   BIGINT UNSIGNED  NOT NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

- One row ≈ 40 bytes. 10M users ≈ 400 MB — fits entirely in InnoDB buffer pool.
- PK lookup by username = O(log n), effectively O(1) in memory.
- Written on signup only (write-once). Read on login only.
- NOT on any post-login hot path (get_user, update_nickname, update_avatar all
  route by `user_id` from the session).

**Redis cache on top:** cache-aside, same pattern as `auth:<username>`.

```
Key:   lookup:<username>
Value: JSON {user_id}
TTL:   10 min (same as auth cache — write-once data, low invalidation cost)
```

Cache hit on login → skip the MySQL lookup table query. Cache miss → query
table, populate cache. Invalidation: only on signup (SET, not DEL — the entry
is being created, not changed). Since the data is immutable once written
(a username maps to one user_id forever), there is no staleness concern and
no need for versioning.

**Why a MySQL table and not purely Redis:**
- Redis is volatile. If Redis dies/flushes, a pure-Redis lookup loses all
  mappings and every login falls through to... nowhere (the per-shard `users`
  tables don't have a global username index). Rebuilding from shards requires
  scanning all shards and collecting `(username, id)` pairs — slow for 10M rows.
- A MySQL table is durable. Redis cache on top is a performance layer — same
  defense-in-depth as the existing `auth:<username>` cache over the `users` table.

**Why a separate, dedicated MySQL instance:**
- Putting the lookup on one of the data shards (e.g. shard 0) creates asymmetric
  load: that shard handles its normal 1/N share of user data queries PLUS 100%
  of all login lookups. The lookups are individually cheap (single PK read, table
  fits in memory), but at 3k-6k login RPS the volume is real and that one shard
  always runs hotter than the others.
- A separate instance keeps every data shard equally loaded and isolates concerns:
  if the lookup instance has a problem, only signup and login are affected —
  post-login operations (get_user, update_nickname, update_avatar) keep working
  because they route by `user_id` from the session and never touch the lookup.
  And if a data shard has a problem, the lookup is unaffected — logins can still
  resolve username→user_id, they just fail at step 3 (the shard query) rather
  than step 1 (the lookup).
- The instance doesn't need to be large — the table is ~400 MB at 10M rows, the
  queries are all PK lookups, and it only handles two operations (INSERT on
  signup, SELECT on login).

**Why the PK is `username` (not `user_id`):**
- The only queries are `SELECT user_id FROM username_lookup WHERE username = ?`
  (login) and `INSERT INTO username_lookup (username, user_id) VALUES (?, ?)`
  (signup). Both are by username. There is no query by user_id on this table.
- The `UNIQUE` constraint that enforces global uniqueness needs to be on
  `username`. Making it the PK means one index instead of two (PK + separate
  unique index), and the lookup query is a PK scan — the fastest possible.

**Scaling the lookup itself (when login volume outgrows one instance):**

The lookup shards independently of the data shards, using a different key:

| Store | Shard key | Access pattern |
|---|---|---|
| Username lookup | `hash(username)` | Every query is `WHERE username = ?` — login and signup |
| User data shards | `hash(user_id)` via consistent hashing | Everything post-login is `WHERE id = ?` |

Each store is sharded by the key it's actually queried on. Two different hash
functions, two independent shard rings. No cross-shard fan-out on either side —
a login lookup hits exactly one lookup shard, then exactly one data shard.

The lookup shards still enforce global uniqueness because each username hashes
to exactly one lookup shard, and that shard's PK constraint catches duplicates.
Two concurrent signups for "alice" both hash to the same lookup shard and the
same PK — one wins, one gets a duplicate key error. No cross-shard coordination
needed.

```
Login flow with both stores sharded:

  1. lookup_shard = hash("alice") % L   → lookup shard 2
  2. lookup_shard_2.GET("alice")        → user_id 12345
  3. data_shard = ShardFor(12345)       → data shard 1
  4. data_shard_1.GetUserByUsername("alice") → {id, hash, row_version}
  5. bcrypt, session (unchanged)
```

### 8.4 Per-Shard Schema

Identical to the current schema except `id` is no longer `AUTO_INCREMENT`:

```sql
CREATE TABLE users (
  id             BIGINT UNSIGNED NOT NULL,       -- snowflake ID, app-generated
  username       VARCHAR(32)  NOT NULL,
  password_hash  VARBINARY(60) NOT NULL,
  nickname       VARCHAR(128) NOT NULL,
  avatar_suffix  CHAR(8)      NULL,
  row_version    BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_username (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

`uk_username` is kept as a within-shard safety net, but global uniqueness is
enforced by the `username_lookup` table. If a bug in the signup flow bypasses
the lookup insert, the per-shard unique index catches duplicates within that
shard (though not across shards — that's the lookup table's job).

### 8.5 Sharded Workflows

#### Signup

```
TCP server (any, via round-robin):

  1. Validate input (unchanged)
  2. bcrypt.Hash(password) (unchanged)
  3. Generate snowflake ID locally (no DB round trip)
  4. INSERT INTO username_lookup (username, snowflake_id)
     → duplicate key? → 409 username taken, stop
     → success? → username globally reserved
  5. shard = ShardFor(snowflake_id)
  6. shard_pool.INSERT(snowflake_id, username, hash, nickname)
     → if this fails: DELETE FROM username_lookup WHERE username = ?
       (rollback the reservation)
  7. [background] cache lookup:<username> → {user_id: snowflake_id}
  8. Return success
```

#### Login

```
TCP server (any, via round-robin):

  1. cache.GET lookup:<username>
     HIT  → have user_id
     MISS → SELECT user_id FROM username_lookup WHERE username = ?
            → not found? → dummy bcrypt verify, return 401
            → found? → cache lookup:<username>, have user_id
  2. shard = ShardFor(user_id)
  3. cache.GET auth:<username>
     HIT  → have {id, password_hash}
            shard_pool.GetRowVersion(user_id) → row_version
     MISS → shard_pool.GetUserByUsername(username)
            → {id, password_hash, row_version}
            [background] cache auth:<username>
  4. bcrypt.Compare (unchanged)
  5. cache.CreateSession(user_id, row_version, ip) (unchanged)
  6. Return token
```

Steps 1-2 are new (the lookup). Steps 3-6 are the existing login flow, just
routed to the correct shard pool instead of the single pool. The auth cache
(`auth:<username>`) and session creation are unchanged.

#### Post-Login (get_user, update_nickname, update_avatar)

```
TCP server (any):

  1. resolveSession(token) → {user_id, row_version}  (unchanged — from Redis)
  2. shard = ShardFor(user_id)                        (one new line)
  3. shard_pool.query(...)                            (unchanged query logic)
```

The username lookup is not involved. Zero extra network hops compared to today.

### 8.6 What Changes in Go Code

| File | Change |
|---|---|
| `db/users.go` | `Handle` becomes a shard-aware type: wraps a `[]Pool` + `lookupPool` + `ShardFor(userID)` router. `InsertUser` no longer reads `LastInsertId()` — takes snowflake ID as a parameter. New `LookupUsername` and `InsertLookup` helpers. |
| `model/user.go` | No change — `User.ID` is already `uint64`. |
| `tcpserver/server.go` | `Server.db` changes from one pool to the shard-aware handle. |
| `tcpserver/auth.go` | `handleSignup`: generate snowflake ID, insert lookup, then insert user. `lookupUserForLogin`: lookup → shard → existing flow. |
| `tcpserver/profile.go` | `handleUpdateNickname`/`handleUpdateAvatar`: shard routing added before the DB call. |
| `cache/user.go` | New `GetLookupCache`/`SetLookupCache` for `lookup:<username>`. |
| `config/config.go` | New `ShardDSNs []string` and `LookupDSN string` config. |
| `cmd/tcp-server/main.go` | Opens multiple pools (one per shard + one for lookup). |

### 8.7 What Does NOT Change

- `row_version` + `LAST_INSERT_ID(row_version + 1)` trick — operates on a single
  row on one shard.
- `avatar_suffix`, file-first avatar ordering, `writeAvatarFile` — filesystem
  sharding (`user_id & 0x1FFF`) is orthogonal to DB sharding.
- Cache keys: `auth:<username>`, `user:<id>:v<N>`, `sess:<token>` — keyed by
  username or user_id, not by shard. Redis doesn't care which MySQL instance
  the data came from.
- Delete-on-write invalidation in `cache/invalidate.go` — targets the same keys.
- Session model — carries `user_id` and `row_version`, both shard-agnostic.
- The custom MySQL pool (`db/pool.go`, `db/conn.go`) — each pool instance is
  unchanged; there are just more of them.
- The HTTP↔TCP multiplexed protocol — completely unaware of sharding.
- The HTTP server entirely — it still round-robins to TCP servers and forwards
  requests. No shard awareness.

### 8.8 Connection Count

MySQL's `max_connections` (default 151) is **per MySQL instance**, not shared
across TCP servers. Each TCP server opens its own connections independently, so
the limit is: `tcp_server_count × pool_size_per_server ≤ max_connections` on
each MySQL instance.

**Per-instance view (what each MySQL instance sees):**

| MySQL instance | Connections from each TCP server | × 3 TCP servers | Total |
|---|---|---|---|
| Data shard 0 | 10 | 30 | 30 |
| Data shard 1 | 10 | 30 | 30 |
| Data shard 2 | 10 | 30 | 30 |
| Lookup instance | 5 | 15 | 15 |

Each data shard sees 30 connections — well within limits. The lookup instance
sees only 15 (lighter pool because its queries are single-row PK lookups with
no bcrypt or file I/O on the critical path).

**System-wide total:** 3 TCP servers × (3 × 10 + 1 × 5) = 105 connections across
all MySQL instances combined. No single instance sees more than 30.

The extra connections from Option A (every TCP server → every shard) are
negligible compared to the operational complexity of an application-aware gateway.

---

### 8.9 Implementation Scope

The sharding design in §8.1–§8.8 is the target architecture. For this entry task,
a subset is implemented to demonstrate the key mechanisms:

**Lookup shards: 2**, routed by `hash(username) % 2`.
- Demonstrates username-based sharding with global uniqueness enforced per-shard.
- Each lookup shard holds ~50% of the username→user_id mappings.
- Both shards are queried/written by all TCP servers (same Option A topology as
  the data shards — every TCP server connects to every lookup shard).

**Data shards: 2 → 3**, starting with 2 and adding a third during the demo.
- Demonstrates consistent-hashing-based routing and the ease of adding a shard.
- With consistent hashing, going from 2 to 3 shards moves ~1/3 of data (only the
  keys between the new node and its predecessor). With simple modulo, the same
  change would reshuffle ~2/3 of all keys.
- The 2 → 3 transition exercises the dual-write migration phases described in §11.

**Localhost setup:** all shards are separate MySQL schemas (databases) on the same
MySQL instance — e.g. `lookup_0`, `lookup_1`, `data_0`, `data_1`, `data_2`. Each
schema contains the same table structure (§8.3 for lookup, §8.4 for data). The
Go code connects to each as a separate DSN with a different database name:
```
data_0:   app:app@tcp(127.0.0.1:3306)/data_0
data_1:   app:app@tcp(127.0.0.1:3306)/data_1
data_2:   app:app@tcp(127.0.0.1:3306)/data_2
lookup_0: app:app@tcp(127.0.0.1:3306)/lookup_0
lookup_1: app:app@tcp(127.0.0.1:3306)/lookup_1
```

The routing, sharding, and migration logic is identical whether pools point at
different ports (real separate instances) or different database names on the same
port. The only thing the localhost setup doesn't demonstrate is physical load
isolation between shards — they share the same CPU, disk, and buffer pool — which
isn't meaningful on a single machine anyway.

**Connection count for this scope:**
- 1 TCP server × (2 data shards × 10 conns + 2 lookup shards × 5 conns) = 30
  connections total.
- Scaling to 3 data shards: 1 × (3 × 10 + 2 × 5) = 40 connections.
- All on the same MySQL instance, which sees all connections against its single
  `max_connections` limit (default 151) — plenty of headroom.

---

## 9. ID Generation for This Project

### 9.1 Decision: Snowflake IDs

64-bit, application-generated, time-sorted IDs. See §4 for the general
reference; this section covers the project-specific choices.

### 9.2 Layout

```
 ┌─────────────────────────────┬──────────┬──────────────┐
 │  41 bits: ms since custom   │ 10 bits: │ 12 bits:     │
 │  epoch (~69.7 years range)  │ machine  │ sequence     │
 │                             │ ID       │ (4096/ms)    │
 └─────────────────────────────┴──────────┴──────────────┘
```

**Custom epoch:** the service launch date (not Unix 1970). Using Unix epoch would
waste ~56 years of the 41-bit timestamp space. A recent epoch maximises the
~69.7 years of usable range.

**Machine ID:** assigned via `MACHINE_ID` env var on each TCP server instance.
For the current scale (1-3 TCP servers), a config value is simplest. If the fleet
grows to dozens of instances, switch to a coordination service (etcd/ZooKeeper
lease). 10 bits supports up to 1024 instances.

**Sequence:** 12-bit counter, resets each millisecond. 4,096 IDs/ms/machine =
4,096,000 IDs/sec/machine — far beyond any realistic signup rate. On overflow,
busy-wait until the next millisecond (sub-ms stall, effectively never happens
for user signups).

### 9.3 Go Implementation

New package `internal/idgen`:

```go
type Generator struct {
    machineID   uint64     // 0–1023, from config
    lastMs      uint64     // last timestamp used
    sequence    uint64     // 0–4095 within current ms
    mu          sync.Mutex // one goroutine at a time
}

func (g *Generator) Next() uint64 {
    g.mu.Lock()
    defer g.mu.Unlock()

    now := millisSinceEpoch()
    if now == g.lastMs {
        g.sequence++
        if g.sequence > 4095 {
            // Exhausted this millisecond — wait for the next one
            for now <= g.lastMs {
                now = millisSinceEpoch()
            }
            g.sequence = 0
        }
    } else {
        g.sequence = 0
        g.lastMs = now
    }

    return (now << 22) | (g.machineID << 12) | g.sequence
}
```

### 9.4 Integration with Existing Code

**`db/users.go` — `InsertUser`:**

Before (single instance):
```go
func InsertUser(ctx, h, username, hash, nickname) (int64, error) {
    res, err := h.ExecContext(ctx, qInsertUser, username, hash, nickname)
    id, _ := res.LastInsertId()
    return id, nil
}
```

After (sharded):
```go
func InsertUser(ctx, h, id uint64, username, hash, nickname) error {
    _, err := h.ShardFor(id).ExecContext(ctx, qInsertUser, id, username, hash, nickname)
    return err
}
```

The ID is generated by the caller (`handleSignup`) before the INSERT. No
`LastInsertId()`. The `qInsertUser` query gains an `id` placeholder:
```sql
INSERT INTO users (id, username, password_hash, nickname) VALUES (?, ?, ?, ?)
```

**`db/users.go` — all other queries:**

`GetUserByID`, `GetRowVersion`, `UpdateNickname`, `UpdateAvatar` already take
`id uint64` as a parameter. The only change is routing to the correct shard pool:
`h.ShardFor(id).QueryRowContext(...)` instead of `h.QueryRowContext(...)`.

`GetUserByUsername` is called after the lookup has already resolved the shard, so
it receives the shard pool directly — no routing change inside the function
itself.

### 9.5 Schema Change

Only one column changes:
```sql
-- Before:
  id  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,

-- After:
  id  BIGINT UNSIGNED NOT NULL,
```

Same type, same width, same PK. B-tree behaviour is identical — snowflake IDs
are time-sorted, so inserts cluster near the rightmost leaf just like
auto-increment.

---

## 10. Rate Limiting for This Project

### 10.1 Decision: Shelved

**Status: considered, documented, not implemented.** Same rationale as the
existing §4.8 in system_design.md — the design has clear plumbing points, and
adding rate limiting later is additive (a check before bcrypt in the login
handler + Redis counter ops), not a restructure.

### 10.2 Why Shelved

- **Correctness cost:** zero — nothing depends on rate limiting.
- **Security cost:** no brute-force protection on login. Acceptable for a
  localhost entry task; noted as production future work.
- **Perf argument** (short-circuit bcrypt on rate-limited attempts) is speculative;
  only helps if the load harness triggers rate limits, which won't happen with
  valid credentials.
- **Time cost** competes with the core deliverables (sharding, perf testing).

### 10.3 Design When Implemented

**Algorithm:** Redis fixed-window counters (§7.3A) for simplicity. Upgrade to
sliding window counter (§7.3C) if boundary bursts are a concern in practice.

**Keys and thresholds (proposed, not implemented):**

| Key | Threshold | TTL | Protects against |
|---|---|---|---|
| `rl:login:ip:<ip>` | 20 failed logins / 5 min | 5 min | Distributed brute-force from one source |
| `rl:login:user:<username>` | 5 failed logins / 15 min | 15 min | Targeted account brute-force |

**Enforcement point:** TCP server login handler, before bcrypt. On a rate-limit
hit, return a new `ErrCodeRateLimited` error code (maps to HTTP 429 in
`httpserver/handlers_auth.go`'s `errorStatus` table).

**Interaction with existing caches:**
- Rate-limit counters are separate Redis keys from `auth:<username>` and
  `sess:<token>`. No interaction.
- A rate-limited request does NOT populate `auth:<username>` (never reached
  MySQL) and does NOT create a session (never passed bcrypt).
- On successful login: `DEL rl:login:user:<username>` to reset the per-user
  counter.

**Timing considerations:** a rate-limited response should still take roughly
bcrypt-duration to return, or an attacker can distinguish "rate-limited" from
"wrong password" by response time. Two options:
- Add a `time.Sleep(bcryptDuration)` before returning 429.
- Or accept the timing leak for a localhost entry task (rate limiting is itself a
  signal that something is wrong — knowing you're rate-limited vs wrong-password
  is low-value information for the attacker).

### 10.4 Why the Design Accepts This Later

The plumbing points are already clear and do not require restructuring:
1. `tcpserver/auth.go:handleLogin` — add a `checkRateLimit(ctx, username, ip)`
   call before `lookupUserForLogin`. Return `ErrCodeRateLimited` if over limit.
2. `protocol/ops.go` — add `ErrCodeRateLimited ErrorCode = "rate_limited"`.
3. `httpserver/handlers_auth.go` — add `"rate_limited": http.StatusTooManyRequests`
   to `errorStatus`.
4. `cache/ratelimit.go` — new file with `IncrLoginAttempt(ctx, username, ip)`
   and `ResetLoginAttempts(ctx, username)`.

Four touch points, all additive. No existing function signatures change. No
existing data flows are rerouted.

---

## 11. Migration Strategy for This Project

### 11.1 Decision: Documented, Not Implemented

The migration strategy is documented here so the design is reviewable, but
no migration tooling is built — there is only one shard (the current single
MySQL instance) and no second shard to migrate to.

### 11.2 How to Add a Shard (When the Time Comes)

Consistent hashing means adding a shard moves ~1/N of users. The migration
follows the dual-write pattern described in §3 of the general reference above,
applied to this project:

**Phase 0 — Before:** all traffic uses the current consistent hash ring (1 shard).
Stand up shard 1 (empty MySQL instance, same schema as §8.4).

**Phase 1 — Dual-write, read-old:**
- `ShardFor(userID)` computes both `old_shard` (v1 ring) and `new_shard`
  (v2 ring with the new node).
- Write path: write to both. For users that didn't move (old == new), this is a
  harmless double-write to the same shard.
- Read path: still reads from `old_shard` only.
- Controlled by a config flag: `MIGRATION_PHASE=dual_write_read_old`.

**Phase 2 — Backfill:**
- Background job iterates the old shard, identifies rows whose `ShardFor_v2`
  differs from `ShardFor_v1`, and copies them to the new shard (upsert by PK).
- Idempotent — safe to restart, safe to run concurrently with live traffic.
- The dual-write from phase 1 ensures nothing written during the backfill
  is missed on the new shard.

**Phase 3 — Dual-write, read-new:**
- Read path switches to `new_shard` (using `ShardFor_v2`).
- Grayscale: 1% → 10% → 50% → 100% of reads from new routing. Monitor error
  rates at each step.
- Config flag: `MIGRATION_PHASE=dual_write_read_new`.

**Phase 4 — Cutover:**
- Stop writing to old shard locations for moved users.
- Remove migration phase config.
- `ShardFor` returns to a simple consistent hash call (v2 ring is now the only
  ring).
- Optionally: delete migrated rows from the old shard to reclaim space.

**Where the code changes live:** `ShardFor(userID)` becomes
`ShardFor(userID, migrationPhase)` during the transition. The handlers
(`auth.go`, `profile.go`) are unchanged — they call `db.GetUserByID` etc. as
always. The routing logic is encapsulated in the shard-aware `Handle` type.

### 11.3 Username Lookup During Migration

The `username_lookup` table is not sharded and does not migrate. It continues to
map every username to its user_id, and `ShardFor(user_id)` handles the rest.
No changes needed to the lookup table or its cache during a migration.

---

## 12. Availability and Replication for This Project

### 12.1 Decision: Shelved

**Status: considered, documented, not implemented.** Same rationale as rate
limiting — the current design accepts replicas cleanly, and building replication
infrastructure for a localhost entry task with one MySQL instance is out of scope.

### 12.2 Why Shelved

- Single MySQL instance serves 10M rows at ~1.3 GB, well within one machine's
  capacity.
- Replication is an infrastructure concern (MySQL config, orchestrator setup),
  not an application concern. The Go code changes are minimal (see §12.4).
- The performance targets (3k-6k login RPS) are achievable on one instance.

### 12.3 What Replication Would Look Like

Each shard gets a primary + one or more replicas:

```
TCP servers (any can reach any shard)
    │
    ├── Shard 0:  Primary ←→ Replica
    ├── Shard 1:  Primary ←→ Replica
    └── Shard 2:  Primary ←→ Replica
```

**Replication mode:** semi-sync (§5.2) — no data loss on failover (≥1 replica
ACKs receipt before the primary commits), at the cost of slightly higher write
latency (one extra network round trip per write for the ACK). Acceptable because
the write paths (signup, nickname update, avatar upload) are infrequent per-user
operations, and the latency is dominated by bcrypt (signup) or file I/O (avatar)
anyway.

### 12.4 Why the Design Accepts This Without Overhaul

**Pool-per-shard naturally splits into write + read pools:**

Currently each shard has one pool. Adding replicas means each shard gets two:
- **Write pool:** points at the primary. Used by `InsertUser`, `UpdateNickname`,
  `UpdateAvatar`, `InsertLookup`.
- **Read pool:** points at the replica (or a load-balanced VIP across replicas).
  Used by `GetUserByID`, `GetUserByUsername`, `GetRowVersion`, `LookupUsername`.

The query helpers in `db/users.go` don't change — the shard-aware `Handle` type
routes writes to the write pool and reads to the read pool based on the query
type. The handlers in `tcpserver/` are unchanged.

**Failover handled by existing retry logic:**

When a primary fails and a replica promotes:
1. Existing connections to the old primary return `driver.ErrBadConn` or EOF.
2. `pool.go`'s `isBadConn` check triggers a retry on a fresh connection.
3. If the DNS/VIP has flipped to the new primary, the fresh `dial()` connects
   to the promoted replica.
4. Callers see a brief latency spike (one failed attempt + retry), not an outage.

No code change in `pool.go` — the retry mechanism already handles this.

**`row_version` cache versioning handles replica staleness:**

If `GetUserByID` reads from a replica that lags behind the primary:
- The replica returns an older `row_version` (e.g. 5 instead of 7).
- The cache is populated at `user:<id>:v5` — stale, but under a key that no
  session will ask for once the session's own hint has been corrected.
- The session's `row_version` hint is corrected on the next write or the next
  cache miss that hits the primary.
- The self-healing mechanism in `handleGetUser` (session hint correction on
  version mismatch) works identically regardless of whether the mismatch came
  from a stale cache or a stale replica.

This is the same class of staleness the cache versioning was designed to handle
— a reader sees an older version, populates the cache under that version's key,
and the system self-corrects. No new failure mode is introduced.

**"Read your own writes" via GTID (future enhancement):**

If replica lag becomes noticeable to users (edit nickname → reload page → see old
nickname because the read hit a lagging replica):
1. On write: capture the GTID from the primary's response.
2. Store the GTID in the session (alongside `row_version`).
3. On read: pass the GTID to the read pool routing layer.
4. The routing layer calls `WAIT_FOR_EXECUTED_GTID_SET(gtid, 50ms)` on the
   replica before querying.
5. If the replica catches up (typically < 1ms) → serve from replica.
6. If timeout → fall back to the primary for this one read.

This is a routing-layer enhancement in the shard-aware `Handle` type. The query
helpers and handlers are unchanged. Not implemented — noted for when replicas
are introduced.

### 12.5 Summary: Design Seams for Future Availability

| Concern | What's needed | What's already in place |
|---|---|---|
| Primary failover | MySQL replication + orchestrator (ProxySQL, Vitess) | `pool.go` retry on `isBadConn` reconnects to the new primary via DNS/VIP flip |
| Read scaling | Read replicas + read/write pool split | Pool-per-shard model extends to pool-pair-per-shard; query helpers unchanged |
| Replica staleness | GTID-based "read your own writes" | `row_version` cache versioning already handles the same class of staleness at the Redis layer; GTID adds the DB-layer equivalent |
| Shard failure | One shard down = fraction of users down | Sharding isolates blast radius; replication within each shard provides per-shard availability |

The design does not need an overhaul to accept any of these. Each is additive:
infrastructure setup (replication config, orchestrator) + a routing-layer
enhancement (read/write pool split, GTID forwarding). The handlers, cache logic,
session model, and invalidation flows are all unchanged.
