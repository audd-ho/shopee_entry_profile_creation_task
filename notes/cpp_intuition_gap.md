# C++ Intuition vs Distributed Systems — Where Low-Level Rules Break

When reasoning about cache consistency strategies (lock vs CAS/atomic, latency
costs, contention), C++ and systems-programming intuitions don't always transfer
to distributed architectures. This document captures where and why they diverge.

---

## 1. Locks vs Atomics — The C++ Rule

In C++, on a hot path touching shared memory:

- **Lock (OS mutex):** kernel syscall → context switch (~1-10μs) → thread
  sleeps → reschedule (~1-10μs). Total overhead: microseconds.
- **Atomic (CAS instruction):** single CPU instruction (~1-10ns). No syscall,
  no sleep, no reschedule.
- **Ratio:** lock is ~100-1000x slower than an atomic. Atomics win decisively.

This is why lock-free data structures dominate C++ hot paths — the cost
differential is enormous and consistent.

---

## 2. Why That Rule Breaks in Distributed Systems

In a distributed system (e.g. Go service + Redis + MySQL), both "lock" and
"atomic" are **network operations**:

| Operation | C++ (in-process) | Distributed (network) |
|---|---|---|
| Lock acquire | ~1-10μs (OS mutex syscall) | ~100-500μs (Redis SETNX round trip) |
| Atomic/CAS op | ~1-10ns (CPU instruction) | ~100-500μs (Redis INCR round trip) |
| Ratio | 100-1000x | **~1x — same order of magnitude** |

The Redis "lock" (SETNX) and the Redis "atomic increment" (INCR) both cost one
network round trip. The CPU instruction cost is irrelevant — network latency
dominates by 3-4 orders of magnitude. A CAS-style version increment is not
cheaper than a lock acquire when both go over the wire.

This means the comparison shifts from **instruction cost** (which matters in
C++) to **system behavior**: what do readers and writers *do* while waiting,
and how does that affect the DB?

---

## 3. CPU Cache Architecture — Coherence, Bouncing, and False Sharing

### Architecture overview

Each core has its own private L1/L2 cache. L3 is shared across cores. Data
moves between RAM and caches in fixed-size chunks called **cache lines**
(typically 64 bytes). You don't cache a single variable — you cache the entire
64-byte block it sits in.

```
Core 0          Core 1          Core 2          Core 3
┌─────┐         ┌─────┐         ┌─────┐         ┌─────┐
│ L1  │         │ L1  │         │ L1  │         │ L1  │
│ L2  │         │ L2  │         │ L2  │         │ L2  │
└──┬──┘         └──┬──┘         └──┬──┘         └──┬──┘
   │               │               │               │
   └───────────────┴───────┬───────┴───────────────┘
                           │
                     ┌─────┴─────┐
                     │  L3 shared│
                     └─────┬─────┘
                           │
                     ┌─────┴─────┐
                     │    RAM    │
                     └───────────┘
```

When Core 0 reads a variable, the entire 64-byte cache line containing it gets
pulled into Core 0's L1. If Core 1 reads the same variable, it gets its own
copy in Core 1's L1. Now two cores have copies of the same data.

The problem: what if one core writes?

```
Core 0 L1: [counter = 5]    (copy)
Core 1 L1: [counter = 5]    (copy)

Core 0: counter = 6         ← writes to its local copy
Core 0 L1: [counter = 6]    (modified)
Core 1 L1: [counter = 5]    (stale — doesn't know about the write)
```

Without a protocol, Core 1 reads 5 when the real value is 6. This is the
coherence problem.

### MESI protocol — each cache line has a state tag

| State | Meaning | What it implies |
|---|---|---|
| **M**odified | This core wrote to it. Only copy. Dirty (RAM is stale). | Must write back to RAM before anyone else can read it. |
| **E**xclusive | This core has the only copy. Clean (matches RAM). | Can write without telling anyone (transitions to M). |
| **S**hared | Multiple cores have copies. Clean. | Must invalidate others before writing. |
| **I**nvalid | Cache line is empty / stale / invalidated. | Must fetch fresh copy from RAM or another core. |

### How the protocol works — the bouncing

```
Step 1: Core 0 reads counter
  - Fetches from RAM → Core 0 L1 = [counter = 5, state: Exclusive]
  - Only one copy exists. Core 0 can read freely.

Step 2: Core 1 reads counter
  - Core 1 requests the cache line
  - Bus snoop: Core 0 sees the request, downgrades its copy
  - Core 0 L1 = [counter = 5, state: Shared]
  - Core 1 L1 = [counter = 5, state: Shared]
  - Two copies, both clean, both can read.

Step 3: Core 0 writes counter = 6
  - Core 0 must get exclusive ownership first
  - Sends invalidation message on the bus
  - Core 1 receives it → Core 1 L1 = [state: Invalid]
  - Core 0 L1 = [counter = 6, state: Modified]
  - Core 1 can no longer read its copy — it's been invalidated

Step 4: Core 1 reads counter
  - Core 1's copy is Invalid → cache miss
  - Bus snoop: Core 0 has it in Modified state
  - Core 0 writes back to RAM (or transfers directly to Core 1)
  - Core 0 L1 = [counter = 6, state: Shared]
  - Core 1 L1 = [counter = 6, state: Shared]
```

That's the bouncing. Every write on one core invalidates every other core's
copy. The next read on those cores is a cache miss — they have to fetch the new
value over the bus. Under contention (multiple cores writing frequently), the
cache line spends most of its time being invalidated and re-fetched rather than
being read from. The data "bounces" between cores' caches via the interconnect
bus.

Cost of each bounce: ~20-100ns per invalidation + re-fetch (depends on whether
it comes from another core's L1/L2 vs L3 vs RAM). Compare to an L1 hit:
~1-4ns. So each bounce is 20-50x slower than a clean local read.

### Memory hierarchy latency reference

```
                          ┌─────────────┐
              ~0ns        │  Registers  │  Directly on the ALU. Zero latency.
                          └──────┬──────┘
                                 │
                          ┌──────┴──────┐
            ~1-4ns        │   L1 Cache  │  32-64KB per core. ~4 cycles.
                          │  (per core) │
                          └──────┬──────┘
                                 │
                          ┌──────┴──────┐
            ~5-10ns       │   L2 Cache  │  256KB-1MB per core. ~10-20 cycles.
                          │  (per core) │
                          └──────┬──────┘
                                 │
                          ┌──────┴──────┐
            ~20-40ns      │   L3 Cache  │  8-64MB shared. ~40-80 cycles.
                          │  (shared)   │
                          └──────┬──────┘
                                 │
                          ┌──────┴──────┐
            ~50-100ns     │    DRAM     │  GBs. ~100-200 cycles.
                          │  (main RAM) │
                          └──────┬──────┘
                                 │
                          ┌──────┴──────┐
            ~100-200ns    │ Remote DRAM │  NUMA cross-socket access.
                          │ (other CPU) │
                          └─────────────┘

Each level is roughly 3-10x slower than the one above.
A cold cache miss that goes to DRAM is ~25-100x slower than an L1 hit.
```

### x86 bus lock vs ARM

On x86, the `LOCK` prefix on an instruction (e.g. `LOCK CMPXCHG`, `LOCK XADD`)
tells the CPU to lock the memory bus (or more precisely, the cache line) for the
duration of that instruction. No other core can read or write that cache line
until the instruction completes. This is how x86 implements atomic
read-modify-write — brute force exclusivity for one instruction.

```
x86 LOCK CMPXCHG (compare-and-swap):

Core 0                          Bus                         Core 1
  │                              │                            │
  ├─ LOCK CMPXCHG ──► lock bus ──┤                            │
  │  read value                  │◄── Core 1 wants access ────┤
  │  compare                     │     STALLED (bus locked)   │
  │  swap (if equal)             │         ...waiting...      │
  │  unlock bus ────────────────►│                            │
  │                              │──── grant access ─────────►│
  │                              │                            │
  Guaranteed atomic.             │          Core 1 proceeds.
  Always succeeds.               │
```

ARM takes a different approach: **LL/SC (Load-Link / Store-Conditional)**,
called `LDXR`/`STXR` on ARM64. Instead of locking the bus:

1. `LDXR` (load-exclusive): reads the value and sets a per-core "exclusive
   monitor" on that cache line.
2. Do your computation.
3. `STXR` (store-exclusive): attempts to write. Succeeds only if no other core
   has written to that cache line since the `LDXR`. If another core did write
   (monitor was cleared), `STXR` fails and you retry from step 1.

| | x86 (`LOCK` prefix) | ARM (LL/SC) |
|---|---|---|
| Mechanism | Lock the cache line, execute atomically | Optimistic: try, detect interference, retry |
| Guarantee | Always succeeds (bus is locked) | Can fail (spurious failure possible), must retry |
| Contention behavior | Other cores stall waiting for bus lock release | Other cores proceed; the loser retries |
| Fairness | Implicit (bus arbitration) | No guarantee — can livelock under extreme contention (rare) |
| Memory ordering | x86 is strongly ordered (TSO) by default — most atomics don't need extra fences | ARM is weakly ordered — need explicit memory barriers (`DMB`, `DSB`) around atomics for ordering guarantees |

```
ARM LL/SC (load-link / store-conditional):

Core 0                          Core 1
  │                               │
  ├─ LDXR (load-exclusive)        ├─ LDXR (load-exclusive)
  │  sets exclusive monitor       │  sets exclusive monitor
  │  read value = 5               │  read value = 5
  │                               │
  │  compute: 5 + 1 = 6           │  compute: 5 + 1 = 6
  │                               │
  ├─ STXR (store-exclusive)       │
  │  monitor still set? YES       │
  │  write 6. SUCCESS.            │
  │  (clears Core 1's monitor)    │
  │                               ├─ STXR (store-exclusive)
  │                               │  monitor still set? NO
  │                               │  FAIL. Must retry.
  │                               │
  │                               ├─ LDXR → read 6
  │                               │  compute: 6 + 1 = 7
  │                               ├─ STXR → SUCCESS, write 7
  │                               │
  No bus lock needed.       Loser retries. Correct result (7).
  But can fail and retry.
```

**Memory ordering — the portability trap:**

The memory ordering difference is the one that catches people moving from x86 to
ARM. x86 has **Total Store Order (TSO)**: stores from one core are seen by other
cores in the order they were issued, and a load cannot be reordered before an
earlier store to the same address. This means most concurrent code "just works"
without explicit fences.

ARM has **weak ordering**: loads and stores can be reordered freely by the CPU
as long as single-threaded semantics are preserved. But other cores may see
writes in a different order than they were issued. Without explicit memory
barriers (`DMB`, `DSB`), this can cause bugs:

```
x86 (TSO) — this works without fences:
  Thread A:              Thread B:
  store x = 1           load r1 = y    // if r1 == 1
  store y = 1           load r2 = x    // then r2 MUST be 1 (TSO guarantees)

ARM (weak) — this can break without fences:
  Thread A:              Thread B:
  store x = 1           load r1 = y    // if r1 == 1
  store y = 1           load r2 = x    // r2 MIGHT be 0! (stores reordered)

  Fix: DMB (data memory barrier) between the stores on Thread A.
```

### Down to assembly — what the CPU actually executes

The difference between x86 and ARM isn't just an API choice — it produces
fundamentally different machine code for the same logical operation.

**Atomic increment — x86 assembly:**

```asm
; x86: atomic increment of [counter]
lock xadd [counter], 1
; One instruction. Bus lock. Full barrier. Done.
; The LOCK prefix makes this atomic AND acts as a full memory fence.
```

**Atomic increment — ARM64 assembly:**

```asm
; ARM64: atomic increment of [counter]
retry:
    ldxr  w0, [counter]      ; load-exclusive: read value, set monitor
    add   w0, w0, #1         ; compute new value
    stxr  w1, w0, [counter]  ; store-exclusive: try to write
    cbnz  w1, retry          ; if w1 != 0 (failed), retry from top
    dmb   ish                ; data memory barrier for ordering
; Five instructions, a retry loop, and an explicit barrier.
```

x86 needs one instruction. ARM needs five plus a retry loop. The `dmb ish`
(inner-shareable data memory barrier) at the end is the explicit ordering
guarantee that x86 gets for free from TSO — without it, other cores might see
the incremented value before they see stores that logically preceded the
increment.

**Why the difference matters at the hardware level:**

The instruction set reflects the CPU's memory model, which reflects a
fundamental hardware design tradeoff:

```
x86 (strong ordering / TSO):
  CPU internally: out-of-order execution, store buffers, speculation
  BUT: hardware enforces that stores appear in program order to other cores
  Cost: silicon dedicated to ordering enforcement, higher power
  Benefit: simpler programming model, fewer bugs from forgotten fences

ARM (weak ordering):
  CPU internally: out-of-order execution, store buffers, speculation
  AND: hardware does NOT enforce cross-core ordering unless told to
  Cost: programmer must insert explicit barriers or use std::memory_order
  Benefit: wider out-of-order windows, less constraining store buffers,
           better throughput, lower power consumption
```

ARM's weaker ordering gives the hardware more freedom to reorder and pipeline
operations — which translates to better performance per watt. This is why ARM
dominates mobile and embedded (power efficiency is critical) and is increasingly
used in servers (AWS Graviton, Apple Silicon) where perf/watt matters at scale.
x86's stronger ordering means fewer concurrency bugs at the cost of more
silicon dedicated to enforcing order.

### What C++ `std::memory_order` does at the assembly level

C++ `std::memory_order` abstractions (`seq_cst`, `acquire`, `release`,
`relaxed`) tell the compiler what ordering you need, and it inserts the right
platform-specific instructions:

| `std::memory_order` | x86 emits | ARM64 emits | Strength |
|---|---|---|---|
| `seq_cst` (default) | `lock` prefix (or `mfence`) | `dmb ish` (full barrier) | Strongest — total order across all cores |
| `acquire` | Nothing (TSO gives this for free on loads) | `dmb ishld` (load barrier) | Reads after this see all prior writes |
| `release` | Nothing (TSO gives this for free on stores) | `dmb ish` (store barrier) | All writes before this are visible to acquirers |
| `relaxed` | Nothing | Nothing | No ordering guarantee at all |

On x86, `acquire` and `release` emit **no extra instructions** because TSO
already guarantees that ordering. The compiler just prevents its own reordering
(a compile-time fence, not a runtime one). On ARM, they emit real barrier
instructions that cost real cycles.

**The portability trap:** `relaxed` emits no barriers on either platform. On
x86, TSO still gives you *some* ordering for free — stores are still seen in
order by other cores, just not guaranteed relative to loads. On ARM, you truly
get *nothing* — loads and stores can be reordered in any way. Code that uses
`relaxed` (or no memory order at all in hand-written assembly) on x86, seems to
work because TSO hides the missing barriers, then breaks on ARM where the weak
ordering exposes them. This is the single most common porting bug from x86 to
ARM.

### Visualizing the bounce under contention

When two cores both write frequently to the same cache line (true sharing),
the line ping-pongs between them:

```
Time ──────────────────────────────────────────────────────►

Core 0 L1:  [M: val=5]  [I]          [M: val=7]  [I]          [M: val=9]
                 │        ▲                │        ▲                │
                 ▼        │                ▼        │                ▼
    bus:     invalidate  fetch        invalidate  fetch        invalidate
                 │        ▲                │        ▲                │
                 ▼        │                ▼        │                ▼
Core 1 L1:  [I]          [M: val=6]  [I]          [M: val=8]  [I]

Each arrow = ~20-100ns of bus traffic. Neither core gets sustained L1 hits.
Both cores spend most of their time waiting for ownership, not computing.
```

At high contention the cache line is in transit more than it is resident —
effective throughput collapses to bus speed rather than core speed.

### False sharing

```cpp
struct Counters {
    int counter_a;  // Core 0 writes this
    int counter_b;  // Core 1 writes this
};
```

`counter_a` and `counter_b` are different variables used by different cores. But
they're adjacent in memory — both fit in the same 64-byte cache line. MESI
operates on entire cache lines, not individual variables. So:

```
Core 0 writes counter_a → invalidates Core 1's copy of the ENTIRE cache line
Core 1 writes counter_b → invalidates Core 0's copy of the ENTIRE cache line
```

Neither core is touching the other's data, but they're bouncing the cache line
back and forth because their unrelated data shares a cache line. Every write on
either core causes a miss on the other. Both run at memory speed instead of
cache speed, for no logical reason.

Fix: pad the struct so each counter sits on its own cache line:

```cpp
struct Counters {
    alignas(64) int counter_a;  // own cache line
    alignas(64) int counter_b;  // own cache line
};
```

Now writes don't interfere. Each core keeps its cache line in Modified state
without invalidating the other.

Visualized:

```
Without padding — false sharing:
┌──────────────────────────────────────────────────────────────────┐
│  Cache line (64 bytes)                                           │
│  [counter_a (4B)] [counter_b (4B)] [counter_c (4B)] ... pad ... │
└──────────────────────────────────────────────────────────────────┘
  Core 0 writes ▲                     ▲ Core 1 writes
                └── same cache line ──┘
                    BOUNCING!

With padding — no false sharing:
┌──────────────────────────────────────────────────────────────────┐
│  Cache line 0: [counter_a (4B)] [padding ................. 60B] │
└──────────────────────────────────────────────────────────────────┘
  Core 0 writes ▲  (only core touching this line — stays Modified)

┌──────────────────────────────────────────────────────────────────┐
│  Cache line 1: [counter_b (4B)] [padding ................. 60B] │
└──────────────────────────────────────────────────────────────────┘
  Core 1 writes ▲  (only core touching this line — stays Modified)
```

### Cache bouncing vs false sharing — the hierarchy

Cache bouncing is the broader phenomenon. False sharing is one cause of it.

| | Cache bouncing (the effect) | False sharing (one cause) | True sharing (another cause) |
|---|---|---|---|
| What | Cache line repeatedly invalidated and transferred between cores | Two cores write different variables on the same cache line | Two cores read/write the SAME variable |
| Necessary? | Depends on cause | No — the data isn't logically shared, just physically co-located | Yes — the data IS shared, bouncing is the correct coherence behavior |
| Fix | Depends on cause | Padding, partitioning, alignment | Reduce sharing (single-writer, partitioning) or accept the cost |
| Example | Any MESI invalidation traffic | `struct { int a; int b; }` where Core 0 writes a, Core 1 writes b | `atomic<int> counter` incremented by both cores |

```
Cache bouncing (cache line moves between cores)
├── True sharing (same variable, different cores — unavoidable if data is shared)
└── False sharing (different variables, same cache line — avoidable with padding/alignment)
```

### Padding tradeoff

```
No padding (dense):
Cache line (64 bytes): [counter_a | counter_b | counter_c | ... | counter_16]
- All 16 counters in one cache line
- One fetch from RAM = all 16 hot in L1
- Great if ONE core reads all 16 frequently
- Terrible if different cores write different counters (false sharing)

Padded:
Cache line 0: [counter_a | 60 bytes padding]
Cache line 1: [counter_b | 60 bytes padding]
- Each counter on its own cache line
- 16 fetches from RAM to get all 16 hot
- Great if different cores write different counters (no false sharing)
- Terrible for spatial locality — wasting 93% of cache capacity on padding
```

Padding solves false sharing but destroys spatial locality. You're wasting
L1/L2 capacity on padding bytes. With a 32KB L1, padding means fewer useful
data items fit — more evictions, more cache misses overall.

**When padding is right:** different cores write to different items frequently
(the false sharing cost dominates).

**When padding is wrong:** one core reads many related items together (the
spatial locality benefit dominates).

### Processes vs threads — where false sharing can happen

Different processes generally do NOT share cache lines. Each process gets its
own virtual address space, which maps to its own physical memory pages (4KB
each). The OS allocates separate physical pages per process. Since cache lines
operate on physical addresses, two processes' data lands on different physical
cache lines even if their virtual addresses happen to look similar.

**Exceptions — when processes DO share physical memory:**
- `mmap` with `MAP_SHARED` (explicit shared memory)
- `shmget` / POSIX shared memory (explicit)
- Memory-mapped files both processes map
- After `fork()`, before either writes (copy-on-write — same physical page
  until one writes, then the OS copies it to a new page)

So false sharing between processes only happens when you deliberately set up
shared memory. It doesn't happen by accident.

Threads within the same process share everything. Same heap, same globals, same
physical pages. False sharing is a thread-level problem, not a process-level
problem.

---

## 4. Context Switches and CPU Cache — Why Locks Are Expensive in C++

### What "cold cache" means for threads

```
Before context switch:              After context switch:
┌─────────────────────┐             ┌─────────────────────┐
│ Core 0 — Thread A   │             │ Core 0 — Thread B   │
│                     │             │                     │
│ L1: [A's hot data]  │  ────────►  │ L1: [B's data]      │  Thread B evicts
│ L2: [A's warm data] │  ctx switch │ L2: [B's data]      │  A's working set
└─────────────────────┘             └─────────────────────┘

Thread A resumes (maybe on Core 2):
┌─────────────────────┐
│ Core 2 — Thread A   │
│                     │
│ L1: [empty/other]   │  Every access = cache miss
│ L2: [empty/other]   │  Must re-fetch from L3/RAM
└─────────────────────┘
```

When a thread runs on a CPU core, the data it touches gets pulled into that
core's L1/L2 cache. On a context switch:

1. Another thread runs on the core, fills L1/L2 with *its* data, evicting the
   original thread's working set.
2. The original thread resumes, potentially on a *different* core entirely.
3. Every memory access is now a cache miss:
   - L1 hit: ~1-4ns
   - L2 hit: ~5-10ns
   - L3 hit: ~20-40ns
   - DRAM: ~50-100ns
   - So a cold miss is ~25-100x slower than a hot L1 hit.

This isn't "process memory is gone" — the data is still in RAM. It's that the
CPU's fast local copy (cache hierarchy) has been evicted. For a hot loop
touching the same struct fields repeatedly, the penalty is severe.

### Registers vs cache — a subtlety

Registers ARE faster than L1 — they're what the CPU directly operates on, with
effectively zero access latency. On context switch, the OS saves all register
values to memory (the thread's kernel stack) and restores them on resume. So
after resuming, registers hold the correct values.

The problem is what those values *point to*. A register holding a pointer to a
struct is restored correctly, but the L1 cache line that pointer references has
been evicted. The first dereference after resume is an L1 miss — potentially
going all the way to DRAM. Register restore is cheap; the first *use* of what
they point to is expensive because the cache is cold.

### The full cost of a context switch

A context switch is not just "save registers, load registers." It triggers a
cascade of costs:

```
Thread A running on Core 0
        │
        ▼
1. Kernel trap (syscall or preemption)           ~100-500ns
        │
        ▼
2. Save Thread A's registers to kernel stack     ~50-100ns
        │
        ▼
3. Scheduler picks Thread B                      ~100-500ns
        │
        ▼
4. Switch page tables (if different process)     TLB flush — ~1-10μs of
        │                                        subsequent misses
        ▼
5. Restore Thread B's registers                  ~50-100ns
        │
        ▼
6. Return to userspace                           ~100-500ns
        │
        ▼
7. Thread B starts executing — L1/L2 cold        ~1-50μs of cache misses
                                                  as working set reloads
```

Steps 1-6 are the direct cost (~0.5-2μs). Step 7 is the indirect cost and is
often much larger — the thread's hot data has been evicted from L1/L2, so every
memory access for the next while is a miss that goes to L3 (~20-40ns) or DRAM
(~50-100ns) instead of L1 (~1-4ns).

### Why this doesn't apply in Go

Go uses goroutines — userspace green threads multiplexed onto a small OS thread
pool by the Go runtime scheduler:

```
Go runtime (userspace):
┌─────────────────────────────────────────────────┐
│  OS Thread (M)                                  │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐         │
│  │Goroutine│  │Goroutine│  │Goroutine│  ...     │
│  │   G1    │  │   G2    │  │   G3    │         │
│  └─────────┘  └─────────┘  └─────────┘         │
│       ▲                                         │
│       │ currently running                       │
│       │ (others parked in run queue)            │
│                                                 │
│  Switching G1 → G2:                             │
│  - Save G1's goroutine stack pointer            │
│  - Load G2's goroutine stack pointer            │
│  - No kernel, no syscall, no TLB flush          │
│  - Same OS thread → same core → L1/L2 warm     │
└─────────────────────────────────────────────────┘
```

- Goroutine park/resume: ~10-50ns (pointer swap in userspace, no kernel)
- No OS context switch → no TLB flush, no cache eviction
- Parked goroutine costs ~2-4KB stack, no CPU time
- When a goroutine resumes, it runs on the same OS thread — CPU cache stays warm

| | C++ thread (OS mutex) | Go goroutine (channel/lock) |
|---|---|---|
| Switch cost | ~0.5-2μs direct + ~1-50μs indirect (cold cache) | ~10-50ns (userspace pointer swap) |
| Cache impact | L1/L2 evicted, cold restart | L1/L2 unchanged, warm |
| Kernel involved | Yes (syscall, scheduler, TLB) | No (Go runtime, userspace only) |
| Stack cost | ~1-8MB per thread (OS default) | ~2-4KB per goroutine (growable) |
| 10,000 concurrent | ~10-80GB stack memory, OS scheduler under pressure | ~20-40MB stack memory, Go scheduler handles easily |

A Go goroutine blocking on a Redis lock does not have the CPU cache penalty
that a C++ thread blocking on an OS mutex does. The scheduling overhead that
makes locks expensive in C++ is largely absent.

---

## 5. Core Pinning, Affinity, and Data Partitioning

If you can guarantee one core owns a region of memory exclusively, you get:

- No false sharing (no other core touches that cache line)
- Cache line stays in Modified state on that core permanently (never invalidated)
- Every access is an L1 hit (~1-4ns)
- No bus traffic at all for that data

### Thread affinity + data partitioning

```
Core 0 owns: users[0..999]      → thread 0 pinned to core 0
Core 1 owns: users[1000..1999]  → thread 1 pinned to core 1
```

Each thread only touches its own partition. No shared memory, no coherence
traffic. This is what high-performance systems (DPDK, game engines, trading
systems) do — partition data by core, not by function.

### Single-writer principle

Only one thread ever writes to a given piece of data. Multiple threads can read
(Shared state, no bouncing on reads). Only the writer ever transitions the
cache line to Modified. Readers only see Shared → Shared transitions (no
invalidation, no miss).

This is weaker than full pinning — readers on other cores still fetch the
line — but eliminates the worst pathology (write-write bouncing).

### Constraints

- **OS can still migrate your thread.** Even with `pthread_setaffinity_np` or
  `sched_setaffinity`, the OS *can* preempt and reschedule. For soft real-time
  (HFT, game loops), this is usually good enough. For hard guarantees, you
  `isolcpus` the core from the kernel scheduler entirely — kernel won't
  schedule anything else there.
- **Shared state still exists.** Even with partitioning, some data is
  inherently shared (global config, shared queues, coordination state). The
  goal is to minimize sharing, not eliminate it. Move shared state to its own
  cache-line-aligned structure, accessed rarely, behind a lock or atomic that
  you accept as a serialization point.
- **NUMA adds another layer.** On multi-socket systems, each CPU socket has its
  own RAM. Core 0 (socket 0) accessing RAM attached to socket 1 costs ~2x a
  local access. So even with pinning, if your data is in the wrong socket's
  RAM, you're slow. You need both core pinning AND memory placement (`numactl`,
  `mbind`).

```
NUMA topology (2-socket system):

┌─────────────────────────────┐    ┌─────────────────────────────┐
│ Socket 0                    │    │ Socket 1                    │
│ ┌──────┐ ┌──────┐ ┌──────┐ │    │ ┌──────┐ ┌──────┐ ┌──────┐ │
│ │Core 0│ │Core 1│ │Core 2│ │    │ │Core 4│ │Core 5│ │Core 6│ │
│ └──┬───┘ └──┬───┘ └──┬───┘ │    │ └──┬───┘ └──┬───┘ └──┬───┘ │
│    └────────┴────┬───┘     │    │    └────────┴────┬───┘     │
│            ┌─────┴─────┐   │    │            ┌─────┴─────┐   │
│            │  L3 (0)   │   │    │            │  L3 (1)   │   │
│            └─────┬─────┘   │    │            └─────┬─────┘   │
│            ┌─────┴─────┐   │    │            ┌─────┴─────┐   │
│            │  RAM (0)  │   │    │            │  RAM (1)  │   │
│            └─────┬─────┘   │    │            └─────┬─────┘   │
└──────────────────┼─────────┘    └──────────────────┼─────────┘
                   │          QPI / UPI link          │
                   └─────────────────────────────────┘

Core 0 → RAM (0):  ~50-100ns  (local)
Core 0 → RAM (1):  ~100-200ns (remote, crosses interconnect)
                    ~2x penalty for remote NUMA access
```

Thread pinned to Core 0 but data allocated on Socket 1's RAM = every access
pays the remote penalty. `numactl --membind=0` or `mbind()` ensures the data
lives on the same socket as the core that uses it.

---

## 6. Mapping CPU Concepts to Distributed Systems

The same fundamental tradeoffs that appear at the CPU cache level reappear at
the distributed systems level, just at different time scales:

```
CPU world:                              Distributed world:

┌──────┐  ┌──────┐  ┌──────┐          ┌──────────┐  ┌──────────┐  ┌──────────┐
│Core 0│  │Core 1│  │Core 2│          │ Server A │  │ Server B │  │ Server C │
│  L1  │  │  L1  │  │  L1  │          │  Redis   │  │  Redis   │  │  Redis   │
└──┬───┘  └──┬───┘  └──┬───┘          └────┬─────┘  └────┬─────┘  └────┬─────┘
   │         │         │                    │             │             │
   └─────────┴────┬────┘                    └─────────────┴──────┬──────┘
            ┌─────┴─────┐                              ┌────────┴────────┐
            │    RAM    │                              │     MySQL       │
            │(src truth)│                              │  (src truth)    │
            └───────────┘                              └─────────────────┘

Cache line = Redis key                 ~1-4ns L1 hit  ↔  ~0.1-0.5ms Redis GET
RAM access = MySQL query               ~50-100ns DRAM ↔  ~1-5ms MySQL SELECT
Bus snoop  = version check / lock GET  ~20-100ns      ↔  ~0.1-0.5ms
```

| CPU concept | Distributed equivalent | Time scale |
|---|---|---|
| L1 cache hit | Redis cache hit | ~1-4ns → ~0.1-0.5ms (~100,000x) |
| Cache miss → RAM | Cache miss → MySQL | ~50-100ns → ~1-5ms (~50,000x) |
| Core pinning (one core owns data) | Shard ownership (one server owns user range) | — |
| Padding (isolate cache lines) | Separate Redis keys per concern (auth vs user cache) | — |
| Dense packing (spatial locality) | Single cache entry with all fields (fewer round trips) | — |
| Single-writer principle | Per-user write lock (one writer at a time) | — |
| NUMA locality | Data locality (Redis and MySQL on same machine vs cross-DC) | — |
| MESI invalidation | Cache key deletion / version bump | — |
| Bus lock (`LOCK` prefix) | Redis distributed lock (SETNX) | — |
| Cache-to-cache transfer | One Redis node forwarding to another | — |

Same tradeoffs at every level — isolate to prevent contention, or co-locate for
access efficiency. The right answer depends on whether your bottleneck is
contention (isolate) or access latency (co-locate).

### CAS/lease as cache bouncing

CAS/lease has three copies of "truth" that must stay in step:

```
Redis version counter:  v3
DB data:                v2 (hasn't committed yet)
Redis cache data:       v1 (hasn't been updated yet)
```

Three components, each at a different version at any given moment. Visualized
as a timeline during a write:

```
Time ──────────────────────────────────────────────────────────────────────►

Redis version:  v1 ─────── INCR ──► v2 ──────────────────────────────── v2
                                 │
MySQL DB:       v1 ──────────────┼── UPDATE ──► v2 ────────────────── v2
                                 │           │
Redis data:     {v1, data_1} ────┼───────────┼── SET ──► {v2, data_2}
                                 │           │        │
                          ┌──────┘    ┌──────┘ ┌──────┘
                          │           │        │
                    Window A    Window B  Window C
                    ver=v2      ver=v2   ver=v2
                    DB=v1       DB=v2    DB=v2
                    data=v1     data=v1  data=v2
                    ALL THREE   TWO OUT  FINALLY
                    DISAGREE    OF STEP  IN SYNC

Any reader during windows A or B sees version mismatch → falls through to DB.
Under frequent writes, the system is ALWAYS in window A or B — never in sync.
```

Just like cache coherence in MESI — multiple copies of the same data, each
potentially at a different state (Modified, Exclusive, Shared, Invalid). When
they're out of step, you need a protocol to reconcile. In CPU cache, that's
MESI bus snooping and invalidation messages bouncing between cores. In
CAS/lease, that's the version comparison on every read — "is my version the
same as the authoritative version?" — and the fallback to DB when it's not.

The "bouncing" is exactly what you see: version increments at Redis, propagates
to DB, propagates to cache data, and during that propagation window every reader
has to check, compare, and potentially fall through. Under write pressure, the
versions are *constantly* out of step across the three components — the system
spends most of its time reconciling rather than serving.

| CPU cache coherence | CAS/lease equivalent |
|---|---|
| Cache line in Core 0's L1 | Redis version counter |
| Cache line in Core 1's L1 | Redis cache data |
| RAM (source of truth) | MySQL DB |
| Write on Core 0 → invalidates Core 1 | INCR version → cache data is now "stale" (version mismatch) |
| Core 1 cache miss → re-fetch from RAM | Reader sees mismatch → falls through to DB |
| Bouncing (constant invalidation under write pressure) | Constant version divergence → constant DB fallback |
| False sharing (unrelated data, same cache line, still bounces) | Orphaned version (INCR succeeds, DB fails, but version still forces all readers to DB — "invalidation" of cache for no real data change) |

### Lock as bus lock

The lock has no state that can be out of step:

```
Lock: exists or doesn't
DB: source of truth
Cache: copy of truth or absent
```

The lock is binary — present or absent. There's no "version 3 of the lock" that
could disagree with "version 2 of the DB." It doesn't carry data, doesn't carry
a version, doesn't need to sync with anything. It's a gate — open or closed.

```
Time ──────────────────────────────────────────────────────────────────────►

Lock:     ──── SETNX ──────────────────────────── DEL ────────────────
               │          LOCKED                    │
               │  No reader can see partial state   │
               │                                    │
MySQL DB: v1 ──┼────── UPDATE ──► v2 ───────────────┼── v2 ───────────
               │                                    │
Redis:    v1 ──┼────── DEL ──► (empty) ─────────────┼── (empty) ──────
               │                                    │
               └─────── critical section ───────────┘
                                                    │
                                              Reader arrives here:
                                              cache miss → DB → gets v2
                                              Correct. No intermediate state.
```

When it's closed, nobody reads or writes. When it's open, DB and cache are
guaranteed consistent (because the writer finished both operations before
opening the gate).

| CPU architecture | Lock equivalent |
|---|---|
| Bus lock (`LOCK` prefix on x86) | Redis SETNX |
| While bus is locked, no other core can access the line | While lock held, readers wait — no partial state visible |
| Expensive but guarantees no bouncing during the critical section | Expensive (extra Redis ops) but guarantees no version divergence |
| Released → all cores see final consistent state | Released → cache and DB are consistent, readers proceed |

The bus lock is the nuclear option in CPU architecture — it stops all other
cores from accessing memory during the operation. Expensive, but no bouncing, no
reconciliation, no intermediate state. Same tradeoff as the Redis lock: more
overhead, but the system spends zero time reconciling inconsistent copies,
because inconsistent copies never exist.

---

## 7. HFT and Order Books — Where C++ Rules DO Apply

### Why atomics matter in HFT when "network is the bottleneck"

The naive objection: if network round trip is ~100μs and a CPU lock costs ~1-10μs,
why optimize the lock? The answer is that HFT has removed the network bottleneck.

**They've minimized network to the physical limit:**
- Colocated servers in the same data center as the exchange, often same rack
- Network round trip: ~1-10μs (not ~100-500μs)
- Kernel bypass (DPDK, FPGA NICs) eliminates OS network stack overhead
- At this point network (~1-10μs) and CPU order book processing (~1-10μs) are
  the same order of magnitude — CPU optimization directly affects total latency

**Tail latency and jitter matter more than average:**
- A lock that usually takes 100ns but occasionally spikes to 10μs (contention,
  context switch) means occasionally losing to a competitor
- Atomics have consistent, predictable latency with no worst-case spikes
- One bad tail event can cost more money than thousands of good ones save

**Competition is relative, not absolute:**
- HFT doesn't need to be "fast enough" — it needs to be faster than the next firm
- If your order book match takes 2μs and theirs takes 1.5μs, you lose the fill
  on every contested order
- When network is at physical limits, the remaining CPU microseconds become the
  competitive margin

**Throughput under burst:**
- Market events (news, large orders) cause thousands of updates in milliseconds
- Locks serialize updates: one at a time, with context switch overhead between each
- Lock-free structures let multiple cores process concurrently without serializing
- Under burst, lock-based throughput collapses; lock-free degrades gracefully

### HFT does NOT batch

Batching amortizes network overhead by accumulating items before sending — a
throughput optimization. HFT wants minimum latency on every single message —
a latency optimization. These are opposite goals. Each order goes out
individually, immediately. Some exchanges batch their outbound market data
(e.g. price updates every 50μs), but the firm's own outbound orders are never
batched.

### HFT runs on x86, not ARM

HFT is almost universally x86 (Intel Xeon). This is not accidental — it
follows directly from the architectural tradeoffs covered in section 3:

**Single-threaded clock speed:** x86 still has the highest single-core
performance at peak boost frequency. HFT cares about latency on one critical
path, not throughput across many cores. ARM optimizes for throughput per watt —
the opposite priority.

**TSO is a feature, not a cost:** x86's strong ordering (TSO) means fewer
barrier instructions, fewer bugs, and more predictable latency. ARM's weak
ordering saves power but adds jitter from barrier instructions (`dmb`) and
LL/SC retry loops (`stxr` failures). On a critical path where every
microsecond matters, an occasional `stxr` retry or a `dmb` stall is
unacceptable jitter.

**`LOCK` prefix is deterministic:** always succeeds, no retry loop. ARM's LL/SC
can spuriously fail under contention — that's a tail latency source HFT can't
tolerate. Even if the retry adds only ~10-50ns, that's 10-50ns of jitter on a
path where firms compete at the microsecond level.

**Ecosystem lock-in:** all the critical tooling is built for x86:
- Intel VTune for profiling
- `perf` hardware counters calibrated for Intel/AMD
- FPGA NICs (Xilinx/Altera) with x86 PCIe drivers
- Kernel bypass stacks (DPDK, Solarflare OpenOnload) optimized for x86
- Exchange colocation infrastructure runs x86

Switching to ARM means requalifying the entire stack — a multi-year effort with
no latency benefit.

**Where ARM DOES win:** servers optimized for throughput per watt — web servers,
databases, batch processing, CI/CD (AWS Graviton, Ampere Altra, Apple Silicon).
These workloads care about "how many requests per dollar/watt," not "how fast is
one single request." Different optimization target, different architecture.

```
Workload spectrum:

Latency-critical          Throughput-critical
(HFT, game engines)       (web servers, databases, batch)
      │                          │
      ▼                          ▼
    x86                        ARM
    - TSO (fewer barriers)     - Weak order (more ILP, less power)
    - LOCK always succeeds     - LL/SC (retry OK, saves power)
    - High single-core GHz     - Many efficient cores
    - Higher power/core        - Lower power/core
    - Predictable latency      - Better throughput/watt
```

### What C++ HFT developers need to know about architecture

For day-to-day HFT C++ work, you don't need to memorize ARM's `DMB`/`DSB`/`ISB`
mnemonics — HFT runs on x86 and you use `std::memory_order` for portability
regardless. What you DO need:

- **Understanding that memory ordering exists and why it matters** — so you
  choose the right `std::memory_order` level and don't accidentally rely on
  TSO-specific behavior that would break if the codebase ever ran on ARM
  (unlikely for HFT, but the mental model matters for correctness reasoning).
- **Reading disassembly** — profilers show x86 assembly. Recognizing `lock
  xadd`, `lock cmpxchg`, `mfence`, and knowing what they cost helps you
  interpret flame graphs and spot contention. You're reading it, not writing it.
- **Cache line awareness** — false sharing, padding with `alignas(64)`, keeping
  hot data dense, core pinning. This is the single biggest performance lever
  after algorithmic improvements. All the concepts from section 3 apply directly
  to HFT C++ code.
- **`perf stat` hardware counters** — cache misses, branch mispredictions, IPC.
  These are x86-specific counters read via `perf` or VTune. Understanding what
  they measure (which requires understanding the cache hierarchy from section 3)
  is what turns profiling from "this function is slow" into "this function is
  slow because of L3 misses from false sharing on this struct."

---

## 8. The Regime Determines Which Optimizations Matter

| Regime | Bottleneck | Lock vs Atomic | What to optimize |
|---|---|---|---|
| In-process C++ (shared memory) | CPU instructions | Atomics win 100-1000x | Instruction count, cache locality, lock-free structures |
| Distributed system (Redis/MySQL) | Network round trips | Same cost (~1x) | Number of round trips, system behavior (do readers hit cache or DB?) |
| HFT (colocated, kernel bypass) | CPU AND network equally | Atomics win (back to C++ regime) | Everything — network is already at physical limits, CPU is the remaining margin |

The C++ intuition "atomics always beat locks" is correct when the bottleneck is
CPU. When the bottleneck is network, both cost one round trip and the question
shifts from instruction cost to system-level behavior. HFT brings the network
cost down to where it matches CPU cost, making both matter again.

In C++, lock's stability doesn't justify its cost (100-1000x slower). At
network level, lock's stability comes at no extra cost (same round trip), and
the bouncing it prevents is orders of magnitude more expensive (each
reconciliation is a network round trip, not a cache-to-cache transfer). The
tradeoff flips.
