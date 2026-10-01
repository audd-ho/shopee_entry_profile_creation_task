# Design Q&A — Learning Notes

Working notes from a design-phase walkthrough of this repo, before implementation
started. Companion to `system_design.md` (which records *decisions*); this file
records *understanding* — specifically the things that were misunderstood and had
to be corrected, and the mental models that replaced them.

Weighted deliberately toward mistakes. The correct explanations are easy to find
in docs; knowing which wrong model you personally reached for is not.

---

## Part 1 — Misconceptions, corrected

Each entry: what was believed, what is actually true, why the wrong model was
tempting, and what it would have cost if it had survived into code.

### 1.1 "The session token is stored in the browser's localStorage"

**What was believed.** Login generates a token, the server hands it to the
client, JavaScript puts it in `localStorage`, and each subsequent `fetch`
attaches it.

**What is actually true.** The token goes into the browser's **cookie jar**, put
there by a `Set-Cookie` response header, and the browser attaches it
automatically to every same-origin request. Client-side JavaScript never sees
it, never stores it, and never sends it explicitly.

```
login   → server responds:  Set-Cookie: session=<token>; HttpOnly; SameSite=Lax; Path=/
        → browser files it in the cookie jar
later   → browser automatically sends:  Cookie: session=<token>
logout  → server responds:  Set-Cookie: session=; Max-Age=-1
```

`HttpOnly` is the load-bearing flag: it makes the cookie invisible to
`document.cookie`, so an XSS bug on any page cannot read the session token. That
protection is impossible with `localStorage`, which is by definition
JS-readable.

**Why the wrong model was tempting.** "Client stores a token and sends it" is
accurate at the level of abstraction — the disagreement is only about *who* does
the storing and sending. Token-in-`localStorage` is also genuinely common in
SPA/JWT designs, so it is a real pattern, just not this one. `system_design.md`
§4.2 chose cookies over JWT.

**What it would have cost.** Building the client to read and attach the token
would have forced `HttpOnly` off, converting every XSS into a full session
compromise. It would also have meant writing manual `Authorization`-header
plumbing in `web/app.js` that the browser does for free.

**Consequence worth knowing.** Because the browser sends the cookie
*automatically*, any other site can cause an authenticated request to be issued
against this app. That is CSRF, and it is the price paid for not using
`localStorage`. `SameSite=Lax` blocks the large majority of it, which is why
§4.9 CSRF is shelved rather than urgent.

---

### 1.2 "The TCP server sets up and maintains the 8 persistent connections"

**What was believed.** Reading `tcpserver.Run`, the accept loop looked like an
ordinary one-connection-per-request server, and the 8 persistent connections
seemed to be missing from it.

**What is actually true.** The number 8 exists **only on the dialing side**. At
startup `cmd/http-server` calls `net.Dial` eight times and keeps those sockets
forever. `cmd/tcp-server` is purely reactive — it loops on `Accept()` and takes
whatever arrives. It has no configuration for the count and never learns it.

```
cmd/http-server                        cmd/tcp-server
  NewPool(addr, 8)                       Run(ctx)
    net.Dial x8   ───────────────────▶     ln.Accept() x8
    8 muxConns, 8 reader goroutines        8 handleConn goroutines
```

Changing `Config.HTTP.TCPPoolSize` from 8 to 32 requires no change to `Run`.

**Where "persistent" actually lives.** At accept time there is no difference
between a persistent and an ephemeral connection. Persistence is a property of
**how long `handleConn` runs**:

- Ephemeral (classic HTTP/1.0): `handleConn` reads one request, writes one
  response, returns. Every request costs a new `Dial` + `Accept`.
- Persistent (this project): `handleConn` contains a `for` loop that decodes
  frame after frame for hours. One `Accept`, thousands of requests.

So the persistence is the `for` loop inside `handleConn`, nothing else.

**Two loops at different levels.** This was the second half of the confusion:

```
Run(ctx)
 └─ for { ln.Accept() }                 ← loop A: runs 8 times, then blocks forever
      └─ go handleConn(c)
           └─ for { cfr.ReadRequest() } ← loop B: 8 of these, one per connection
                └─ go dispatch(req)      ← thousands over the process lifetime
```

Loop A wakes 8 times in the program's entire life and then parks. "Infinite
loop" here means *parked*, not *spinning* — the runtime deschedules the
goroutine on the listener fd via epoll. An idle server with 8 connections burns
zero CPU across all 9 of those goroutines.

**Why keep the accept loop alive at all, then?** Three reasons, in increasing
order of importance:

1. **Reconnection.** A connection dies (network blip, middlebox reaping an idle
   socket) and the pool redials. Without a live accept loop the pool permanently
   drops to 7.
2. **Restarting the frontend.** `cmd/http-server` gets restarted constantly
   during development and dials 8 fresh connections each time.
3. **Horizontal scale.** Three HTTP server instances behind a load balancer
   produce 24 connections, with no change to the TCP server — precisely because
   it never assumed 8.

**Wrinkle.** "Exactly 8" is a steady-state claim, not an invariant. TCP does not
reliably notice a vanished peer, so a SIGKILLed client can leave a half-open
connection whose `handleConn` blocks forever while the client redials — leaking
one connection per incident. Defenses are TCP keepalive or an application-level
idle timeout via read deadlines.

---

### 1.3 "Nesting a package inside another avoids the import cycle"

**What was believed.** A top-level `handler/` package would collide with
`httpserver`, but `internal/httpserver/handlers/` — being *inside* it — would
not.

**What is actually true.** Go does not care about directory nesting.
`internal/httpserver/handlers` and `internal/httpserver` are two entirely
unrelated packages that happen to share a path prefix. A child may not import
its parent any more than a stranger may. The cycle is identical:

```
handlers  →  httpserver   (needs the TCP client pool)
httpserver →  handlers    (needs them for the route table)
```

**The two real fixes.**

*Consumer-side interface (no new package).* `handlers` declares what it needs
instead of importing the concrete thing:

```go
package handlers

type Backend interface {
    Do(ctx context.Context, req protocol.Request) (protocol.Response, error)
}
```

`httpserver` constructs the real pool and passes it in. This is the idiomatic Go
move — "accept interfaces, return structs" — and it makes handlers testable with
a fake backend.

*Move the pool out.* `internal/tcpclient/` as its own package, so
`handlers → tcpclient` and `httpserver → handlers`, acyclic.

**The hidden cost that settled the question.** The split cascades. `session.go`'s
cookie helpers are needed by both the login handler *and* the session
middleware; if they stay in `httpserver`, `handlers` can't reach them and the
cycle returns. Same for the context-key getters. The end state is four packages
where there was one — for seven endpoints. Deferred.

**Related Go convention worth absorbing.** `handler/` + `service/` +
`repository/` groups by *layer* and is a Java/Spring habit. Go groups by
*responsibility*: `validate`, `crypto`, `cache` are things, not tiers. The
stdlib has no `net/http/handlers` package.

---

### 1.4 "A buffered channel is lock-free, so it beats a mutex"

**What was believed.** Swapping `sync.Mutex` for a buffered channel as a
connection free-list would eliminate locking.

**What is actually true.** `runtime.hchan` — the struct behind every channel —
contains a `lock mutex` field, and **every** send and receive acquires it, even
when the buffer has room. Channels relocate the lock into the runtime; they do
not remove it.

**The more useful correction.** An uncontended `sync.Mutex.Lock()` compiles down
to a single `CompareAndSwapInt32` on the state word. No syscall, no parking, no
kernel. It only escalates to spinning and futex-parking when the CAS fails. So:

| operation | rough cost |
|---|---|
| uncontended `sync.Mutex` lock+unlock | ~15 ns |
| buffered channel send+recv | ~25 ns |
| a hand-rolled lock-free CAS | ~one atomic |

**You would be replacing one atomic operation with one atomic operation.** The
word "mutex" imports a lot of assumed cost from other languages that does not
apply in Go.

**What genuinely lock-free would look like, and what it loses.** A Treiber stack
(CAS on a head pointer) is real lock-free, and Go's GC defuses most of the ABA
problem that makes these treacherous in C. For a fixed set of 8, an even simpler
option is an atomic flag per connection with a round-robin probe:

```go
// each muxConn has: writing atomic.Bool
start := rr.Add(1)
for i := range conns {
    c := conns[(start+uint64(i)) % uint64(len(conns))]
    if c.writing.CompareAndSwap(false, true) { return c }
}
```

What both lose is **free parking**. When every connection is busy, a lock-free
pop returns "empty" and hands the problem back — leaving spinning (burns a core)
or reintroducing a channel/condvar to block on. That convenience is most of what
the 25 ns buys.

**The decisive argument: run the numbers.** Eight connections, each held for the
microseconds it takes to buffer and flush a few hundred bytes. At 6k RPS with a
15 ns lock pair, that is ~90 µs of lock overhead per second — about **0.009% of
one core**. Lock-free might halve it. Meanwhile bcrypt is ~60 ms per login.
Every mutex in this program combined is invisible next to that.

Lock-free structures earn their complexity under sustained high contention
across many cores. This workload has neither.

**Also:** do not reach for `sync.Pool` for connections. It is mostly lock-free
with per-P caches and looks superficially right, but it can drop items at any GC
and gives no ordering guarantees. Fine for reusable buffers, wrong for
connections that must not be lost.

---

### 1.5 "Middleware and multiplexing are the two most complex parts"

**What was believed.** These two sat together at the top of the difficulty list.

**What is actually true.** Multiplexing belongs there. Middleware does not — it
is roughly 60 lines, mechanical, written once and never revisited. Honest
ranking by risk:

| Rank | Component | Why |
|---|---|---|
| 1 | `tcpclient.go` multiplexing | Concurrency bugs whose failure mode is a *hang*, not a crash |
| 2 | `db/pool.go` | Same class of bug, simpler semantics (exclusive checkout) |
| 3 | Perf days 9–11 | Not code complexity; open-ended investigation against a hard number |
| 4 | Avatar write path | Real race across version bump, file write, cache invalidation (§4.4.4) |
| 5 | TCP server lifecycle | Hand-rolled drain; works in dev, breaks under load |
| 6 | Cache invalidation | Delete-on-write is mostly safe; read-during-write needs thought |
| 7 | Middleware | Mechanical |

**The one real trap in middleware.** `http.ResponseWriter` is write-only — there
is no `Status()` getter — so a logger that wants to record the status code must
wrap it:

```go
type statusRecorder struct {
    http.ResponseWriter
    status int
}
func (r *statusRecorder) WriteHeader(code int) {
    r.status = code
    r.ResponseWriter.WriteHeader(code)
}
```

Two sub-traps: initialise `status: 200`, because a handler that never calls
`WriteHeader` gets an implicit 200 on first `Write` and you would otherwise log
zeros; and embedding only promotes `ResponseWriter` methods, so a real writer's
`http.Flusher` / `http.Hijacker` are silently hidden by the wrapper. Harmless
for JSON APIs, fatal for streaming.

**Why multiplexing is genuinely hard — the specific failure modes:**

- **Unbuffered response channels (the killer).** The reader receives a response,
  looks up `pending[id]`, sends — but the caller already timed out and walked
  away. Nobody is receiving. The reader blocks forever, and it is the *only*
  thing draining that connection, so every other in-flight request on it wedges
  too. One abandoned request kills the connection. Fix: **buffer size 1**, so the
  send always completes and the value is dropped.
- **Cleanup on cancellation.** A caller whose deadline fires must delete its own
  `pending` entry, or the map leaks one entry per cancelled request.
- **Connection death must fail *all* waiters.** The reader, on exit, must walk
  `pending` and close every channel. Otherwise one connection failure becomes
  thousands of individual 2-second stalls.
- **Lock ordering.** `writeMu` and `pendingMu` live on the same struct. Never
  acquire one while holding the other.
- **Reconnect idempotency.** Retrying `login` on another connection is safe;
  retrying `signup` could duplicate. The unique index on `username` saves you,
  but it surfaces as a confusing "username taken".

---

### 1.6 "Pass the writer and its mutex as two parameters"

**The original shape:**

```go
func EncodeRequest(w io.Writer, mu *sync.Mutex, req *Request) error
```

**Three problems, in increasing severity.**

1. **Nothing enforces the pairing.** `EncodeRequest(connA, &muB, req)` compiles
   and runs. The mutex's entire job is to guard *that specific writer*, but as
   two independent parameters the relationship exists only in the author's head.
2. **`if mu != nil` makes locking optional.** This is the real hazard — it
   invites a caller to pass `nil`, reasonably, for a single-threaded path. The
   moment that writer is shared, frames splice on the wire. It works perfectly
   in dev and corrupts under concurrency, the worst available failure profile.
3. **A mutex should live next to what it guards.** This is the standard Go idiom
   precisely because it is the only way to make the association checkable.

**The corrected shape.** Since the writer never changes, the encoder can be
hoisted onto the struct too:

```go
type ConnectionFrameWriter struct {
    mu  sync.Mutex
    bw  *bufio.Writer
    enc *json.Encoder
}

func NewConnectionFrameWriter(w io.Writer) *ConnectionFrameWriter {
    bw := bufio.NewWriter(w)
    return &ConnectionFrameWriter{bw: bw, enc: json.NewEncoder(bw)}
}

func (cfw *ConnectionFrameWriter) WriteRequest(req *Request) error {
    cfw.mu.Lock()
    defer cfw.mu.Unlock()
    if err := cfw.enc.Encode(req); err != nil {      // JSON + '\n'
        return fmt.Errorf("encode request: %w", err)
    }
    return cfw.bw.Flush()                             // must be inside the lock
}
```

Three incidental fixes folded in: `json.Encoder` appends the newline itself (so
`append(b, '\n')` and its reallocation disappear); `bufio.Writer` avoids a
syscall per frame; and the two near-identical methods can share one unexported
`write(v any)`.

**One deliberate trade.** `Encode` marshals *inside* the critical section, where
the original marshalled outside it. The original is better on paper — shorter
hold — but at the cost of serializing a 2.7 MB base64 avatar under the lock if
you go the other way. Not worth pre-optimizing; just know which knob it is.

**Why both `enc` and `bw` are stored.** `json.Encoder` exposes no accessor for
the writer it wraps, so a separate reference is needed to call `Flush()`. That is
the only reason the struct has two fields.

---

### 1.7 "Where is the conn? Does the encoder automatically pipe to it?"

**The confusion.** `WriteRequest` has no writer parameter and the struct has no
`conn` field, so it was unclear how bytes reach the network.

**What is actually true.** `net.Conn` *is* an `io.Writer` — its interface
includes `Write([]byte) (int, error)`, which is the whole requirement. So
`NewConnectionFrameWriter(conn)` puts the conn in the `w io.Writer` parameter,
and it ends up nested inside `bw`.

**The wiring happens once, at construction:**

```
ConnectionFrameWriter
  └── enc (*json.Encoder)  ──holds──▶ bw
        └── bw (*bufio.Writer) ──holds──▶ conn (net.Conn)
              └── conn               ──────▶ network
```

**There is no "piping" — `Encode` just calls `Write`.** Stripped down,
`json.Encoder` is:

```go
type Encoder struct { w io.Writer }

func (e *Encoder) Encode(v any) error {
    b, err := json.Marshal(v)
    if err != nil { return err }
    b = append(b, '\n')
    _, err = e.w.Write(b)      // ← the entire "pipe"
    return err
}
```

That is the original `EncodeRequest`, with the writer remembered in a field
instead of passed as an argument. Same code, different place to keep the
reference.

**Following the bytes through one call:**

```
fw.enc.Encode(req)   → marshals, appends '\n', calls bw.Write(...)
                     → bytes land in a 4 KB in-memory buffer
                     → NOTHING has left the process yet
fw.bw.Flush()        → bw calls conn.Write(buffered bytes) — one syscall, bytes go out
```

Which is exactly why the `Flush` is mandatory, and why omitting it presents as a
network hang rather than an error.

**Should the struct also hold the conn?** Only if it needs conn-specific methods
— `SetWriteDeadline`, `Close`, `RemoteAddr` — and `bufio.Writer` exposes none.
It doesn't need them; that job belongs to `muxConn`, which holds the `net.Conn`
for lifecycle and a `*ConnectionFrameWriter` for the write path. Two owners, two
concerns.

**The payoff of accepting `io.Writer` over `net.Conn`:** `frame_test.go` needs
no network.

```go
var buf bytes.Buffer
fw := NewConnectionFrameWriter(&buf)
fw.WriteRequest(req)
// buf.String() is the exact bytes that would have gone on the wire
```

---

### 1.8 "The reader can't be symmetric with the writer, so the design doesn't work"

**Where this got stuck.** `ConnectionFrameWriter` has both `WriteRequest` and
`WriteResponse`, but the reader side seemed to resist the same treatment,
because the HTTP and TCP sides clearly do different things with a decoded frame.

**What is actually true.** The *reader type* is symmetric. What is asymmetric is
the **loop**, and that belongs outside `protocol` entirely.

```go
type ConnectionFrameReader struct {
    dec *json.Decoder    // no mutex — one reader goroutine per connection
}

func (cfr *ConnectionFrameReader) ReadRequest(req *Request) error   // tcp side
func (cfr *ConnectionFrameReader) ReadResponse(resp *Response) error // http side
```

Two methods, each side uses one — exactly like the writer. `WriteRequest` +
`ReadResponse` on the HTTP side; `ReadRequest` + `WriteResponse` on the TCP side.

**The loops, which are genuinely different, live in each tier:**

```go
// tcpserver/conn.go — decode, hand off, handler writes back
for {
    var req protocol.Request
    if err := cfr.ReadRequest(&req); err != nil { break }
    go func(req protocol.Request) {
        cfw.WriteResponse(dispatch(reqCtx, req))   // cfw captured by closure
    }(req)
}

// httpserver/tcpclient.go — decode, match, wake the waiter
for {
    var resp protocol.Response
    if err := cfr.ReadResponse(&resp); err != nil { break }
    c.pendingMu.Lock()
    ch := c.pending[resp.ReqId]
    delete(c.pending, resp.ReqId)
    c.pendingMu.Unlock()
    if ch != nil { ch <- resp }    // buffered size 1, never blocks
}
```

`protocol` owns the wire format; each tier owns its dispatch policy. Trying to
put the loop in `protocol` is what made the design feel broken — the package
would have to know about `dispatch` and `pending`, which are tier concerns.

**"Do I have to pass the connection to another function?"** You pass the
`*ConnectionFrameWriter`, not the conn — and usually you don't pass it at all.
The handler goroutine is a closure created inside `handleConn`, so `cfw` is in
scope lexically.

**Ownership summary:** one `net.Conn` → one reader + one writer, created
together at accept/dial, living for that connection's lifetime, shared by all
that connection's handler goroutines.

---

### 1.9 "A central goroutine should manage the 8 connections and route responses"

**What was believed.** A "center" takes requests off a channel, distributes them
across the 8 connections, and routes responses back — otherwise a round-robin
`% 8` might land on a connection that is mid-write and stall.

**What is actually true.** There is no center, because **nothing is ever
"busy"**. Writing a request does not wait for its response: buffer, flush,
return, in microseconds. A connection with 50 responses outstanding is no less
available than an idle one. With nothing to arbitrate, connection selection is
pure load spreading:

```go
i := p.rr.Add(1) % uint64(len(p.conns))
c := p.conns[i]
```

One atomic increment. A central dispatcher would add a hop and become a
bottleneck — every request funnelling through one goroutine is strictly worse
than each picking its own.

**The actual goroutine anatomy:**

```
HTTP handler goroutines (hundreds)
   │  pick conn → register in pending → write frame → block on own channel
   ▼
 muxConn ×8
   └─ reader goroutine ×8
        loop: decode response → pending[req_id] → send on channel
```

**"That's a lot of channels."** A buffered channel of size 1 is ~96 bytes. At 3k
RPS with 2 ms round-trips there are ~6 in flight. Even 10,000 concurrent
in-flight requests is about a megabyte. The polling alternative is strictly
worse: busy-waiting burns CPU and still needs synchronization. Blocking on a
channel costs zero CPU — the runtime parks the goroutine and wakes it on send.

**TCP server steady-state goroutine census**, for grounding (k = in-flight
requests):

| Count | Goroutine | State |
|---|---|---|
| 1 | accept loop | parked in `Accept()` |
| 1 | ctx watcher | parked on `<-ctx.Done()` |
| 8 | `handleConn` | parked in `ReadRequest()` |
| k | request handlers | actually working — bcrypt, SQL, Redis |

Only the bottom row does anything.

---

### 1.10 "Responses could come back on any connection"

**What was believed.** A shared response pool where any connection can carry any
response, with the client matching by `req_id`, would relieve congestion.

**What is actually true.** The response always returns on the **same**
connection. TCP is bidirectional and the server writes its reply to the socket
it read the request from — there is no routing decision. Four reasons the
alternative is a bad trade:

1. **The server does not know the 8 connections are one client.** Decisive.
   From the TCP server's view these are 8 independent connections that happen to
   share an IP. Cross-connection routing would need a handshake announcing pool
   identity, a server-side registry of live connections, and a policy for a
   return connection dying mid-flight.
2. **`req_id` would have to become globally unique.** Today each `muxConn` has
   its own counter, so conn A and conn B can both have an in-flight #5. A shared
   return path forces one ID space and one `pending` map behind one mutex,
   touched twice per request — adding the hottest lock in the system to remove a
   lock that currently doesn't contend.
3. **It targets the wrong direction.** The congestion is client→server (2.7 MB
   base64 uploads). Responses are all small.
4. **It wouldn't add throughput.** Eight connections to the same host share the
   same path and bandwidth.

Worth noting the industry converged here: HTTP/2, gRPC, and QUIC all return a
stream's response on its own connection. Nobody does cross-connection routing.

**What isn't one-to-one** is *ordering*: conn 3 may have requests 7, 8, 9
outstanding and see replies as 9, 7, 8 — say 8 hit a Redis cache in 0.2 ms while
7 ran bcrypt for 80 ms. That reordering is exactly what `req_id` untangles. Two
consequences: `pending` is **per-connection**, and `req_id` need only be unique
**per connection** (a plain `atomic.Uint64` per `muxConn`).

**If more write parallelism is wanted, just raise the connection count.** 8 → 32
is a config change (`Config.HTTP.TCPPoolSize` already exists), costs a few KB of
buffers each, and gives linearly more write slots with no new machinery.

---

### 1.11 "Build the HTTP server and all routes first, add logging later"

**The original plan.** HTTP server + all routing endpoints → TCP connection →
Redis and DB → business logic → features like logging and extra middleware.

**Two problems.**

*Logs last.* Self-caught during the discussion, and correctly. Logging, request
IDs, and panic recovery are not features — they are the instrument panel used to
debug everything else. About 60 lines, and they pay for themselves at the first
mysterious 500.

*Breadth before depth.* Building all seven routes before any TCP connection
exists means each returns a stub and each gets revisited once the real backend
lands. Worse, integration problems surface only at step 4 — and the riskiest
code in the project (multiplexed response correlation) lives exactly there. Far
better to debug that when the payload is the string `"ping"` than when it is
tangled with bcrypt, MySQL, and cookies.

**The corrected principle:** one route, all the way down, as early as possible.
Then thicken. See Part 4 for the resulting phase order.

---

### 1.12 "Passing a connection-lifetime context into request handlers"

**What felt wrong, correctly.** `handleConn(ctx, c)` passing that same `ctx`
into `dispatch(ctx, req)` — one context spanning a connection that serves
thousands of requests.

**What is actually true.** A context is scoped to a **unit of work**, not a
connection. Contexts form a tree, each node matching a real lifetime:

```
server ctx                   ← cancelled by SIGTERM
 └── conn ctx                ← cancelled when this connection closes
      ├── request ctx  (+2s deadline, +trace id)
      ├── request ctx  (+2s deadline, +trace id)
      └── request ctx  ...
```

**What collapsing them costs:**

- **No deadlines anywhere.** Follow where ctx is actually *used* — the I/O
  boundaries: `db.Acquire(ctx)`, the go-redis calls, `pool.Do(ctx, req)`.
  `db/pool.go` specifies a ~200 ms wait-queue timeout and `tcpclient.go` a 2 s
  request deadline; both are implemented by the context carrying a deadline.
  With only a SIGTERM context, a hung MySQL connection blocks until the process
  dies.
- **Nowhere to put the trace ID.** A request ID is per-request by definition.
  It cannot be attached to a connection-lifetime context, because `WithValue`
  returns a *new* context — otherwise every request on conn 3 would share one ID.
- **No cleanup signal.** `cancel()` is what releases the timer and deregisters
  the child.

**The corrected shape:**

```go
func (s *Server) handleConn(ctx context.Context, c net.Conn) {
    connCtx, cancelConn := context.WithCancel(ctx)
    defer cancelConn()          // connection dies → its in-flight requests die
    defer c.Close()

    cfr := protocol.NewConnectionFrameReader(c)
    cfw := protocol.NewConnectionFrameWriter(c)

    for {
        var req protocol.Request
        if err := cfr.ReadRequest(&req); err != nil { return }
        go func(req protocol.Request) {
            reqCtx, cancel := context.WithTimeout(connCtx, 2*time.Second)
            defer cancel()
            cfw.WriteResponse(dispatch(reqCtx, req))
        }(req)
    }
}
```

`Run`'s ctx is still passed in, but as the **root of the tree**, not as the
request's context.

**Why it felt weird here and not on the HTTP side.** `net/http` creates a
per-request context automatically and hands it over as `r.Context()`, already
scoped correctly. On the TCP side there is no framework doing that — this is
hand-rolling what `net/http` does internally, which is a recurring theme in this
project alongside the custom pool, the framing, and the middleware.

---

### 1.13 Context mechanics: the apparent contradiction

**The confusion.** Two statements seemed to conflict: that attaching per-request
values to a shared context would mean "mutating shared state," and that
`connCtx` nevertheless earns its place.

**Resolution.** They are about different things. `connCtx` exists purely as a
**cancellation scope** and carries no per-request values. Per-request values go
on `reqCtx`, derived from it. The "mutating" remark was an argument *for*
deriving per-request contexts, not against having a connection-scoped one.

**Derivation never touches the parent.** `context.WithValue(parent, k, v)`
allocates a **new** node holding `k`, `v`, and a pointer *to* parent:

```
                     ┌─ reqCtx#1 {trace: "a1b2"} ──▶ connCtx
connCtx ◀────────────┼─ reqCtx#2 {trace: "c3d4"} ──▶ connCtx
                     └─ reqCtx#3 {trace: "e5f6"} ──▶ connCtx
```

Three siblings, three trace IDs, one shared parent, zero mutation.

**The asymmetry that makes it all work:**

| | Direction | Mechanism |
|---|---|---|
| **Values** | resolved **upward** (pull) | child holds a pointer to parent; parent tracks nothing |
| **Cancellation** | propagates **downward** (push) | parent holds a `children` set and actively cancels each |

So `ctx.Value(k)` walks toward the root, returning the first hit — which is why
a child sees every ancestor's values and an ancestor sees none of its children's,
and why contexts should stay shallow (each `WithValue` adds a hop to every
lookup). Conversely `WithCancel` **registers** the child in the parent's
`children` map, so cancelling the root closes every descendant's `Done()`
channel recursively. Descendants find out without "accessing" the root.

**But cancellation is cooperative — nothing is interrupted.** Closing `Done()`
only wakes code that is *selecting on it*. Two live examples in this project:

- **The shutdown bug.** `cfr.ReadRequest` blocks in a socket read, which selects
  on nothing. Cancelling the whole tree leaves that goroutine sitting there. This
  is exactly why the connection fd must *also* be closed — cancellation and fd
  closure are separate mechanisms, and blocking I/O only responds to the second.
- **bcrypt.** ~60 ms of pure CPU, takes no context. A request cancelled at t=0
  still burns the full 60 ms; the result is simply discarded.

**The leak this creates in exactly this setup.** Since `reqCtx` registers itself
in `connCtx`'s children map, and `connCtx` lives for hours, **forgetting
`defer cancel()` leaks one entry per request** — never collected, because the
parent still holds the reference. `cancel()` is what deregisters the child, and
it is safe to call more than once, so `defer cancel()` immediately after every
`WithTimeout`/`WithCancel` is unconditional. Enable `go vet`'s `lostcancel`
check in CI.

---

### 1.14 False alarm: the `okResponse` double-marshal

Recorded because the worry was reasonable and the reasoning is worth keeping.

**The worry.** `okResponse` calls `json.Marshal(data)` and stores the result in
`Response.Data`, and then the whole `Response` is marshalled again by
`enc.Encode`. Double encoding?

**No — because `Data` is `json.RawMessage`.** Its `MarshalJSON` hands the bytes
back unchanged:

```go
func (m RawMessage) MarshalJSON() ([]byte, error) {
    if m == nil { return []byte("null"), nil }
    return m, nil
}
```

The encoder sees a type implementing `json.Marshaler`, calls it, and **splices
the bytes straight into the output**. No re-escaping, no re-parsing:

```
json.Marshal(data)      →  {"user_id":42,"nickname":"bob"}
enc.Encode(resp)        →  {"req_id":7,"status":"ok","data":{"user_id":42,"nickname":"bob"}}
```

**The bug that was being sensed is real — just avoided by the type choice.**
Declaring the field as plain `[]byte` breaks it immediately, because
`encoding/json` base64-encodes byte slices:

```
{"req_id":7,"status":"ok","data":"eyJ1c2VyX2lkIjo0Mn0="}
```

Valid JSON, completely unusable. `json.RawMessage` exists precisely to avoid
this.

**It is one marshal, not two.** The payload is serialized exactly once; the
encoder's step is a `memcpy` into the output buffer. The only cost of the
`RawMessage` route is one extra `[]byte` allocation — so the alternative
(`Data any`, marshalled inline by the encoder) costs about the same. The choice
is therefore not about performance but about **error timing**: with `Data any`, a
marshal failure surfaces inside `enc.Encode` with no response left to send, so
the client waits out its full 2 s timeout. With `RawMessage`, the failure
surfaces in `okResponse` while it can still be converted into a valid
`errResponse` and sent immediately.

---

## Part 2 — Real gaps found in the scaffold

These were defects in the design/stubs, not misunderstandings.

### 2.1 No session-validation op

`protocol/ops.go` defines six ops — `signup`, `login`, `logout`, `get_me`,
`update_nickname`, `update_avatar` — and none of them resolves a token to a
user. But `middleware.go` specifies "look up session in Redis **via TCP
server**", and `tcpserver/auth.go`'s `handleGetMe` states "user_id already known
from session (passed in data)". So something upstream must have already done the
resolution, and there was no op for it.

**Options weighed:**

| | Round-trips | Notes |
|---|---|---|
| (a) Add `OpValidateSession` | 2 per authed request | Simplest, matches the stubs as written |
| (b) Token in every op's `data`, resolved TCP-side | 1 | Middleware degrades to "extract cookie, 401 if absent" |
| (c) Give HTTP server its own Redis client | 1 | Breaks the tier boundary |

**Decided: (b).** Preserves the property that Redis lives only in the TCP tier,
halves the round-trips, and the cost is a three-line session lookup at the top
of each TCP handler — trivially factored into a helper in `dispatch.go`.

### 2.2 Middleware chain order was wrong

`middleware.go` listed `Recover` (1), `Logger` (2), `RequestID` (3). Two issues:
the logger runs *outside* the thing that generates the ID, so log lines cannot
contain it; and with `Recover` outermost, a panic unwinds through `Logger`
before `Recover` writes the 500, so the logged status is wrong.

**Corrected order:** `RequestID → Logger → Recover → Session → [RequireAuth]`.

Also: `RequireAuth` is not a chain member. `routes.go` marks five endpoints as
auth-required and three as not, so it is a per-route wrapper applied
selectively. And the whole chain should skip `/uploads/*` and the static
HTML/CSS/JS, or every avatar image fetch pays a session round-trip through a
pool sized at 8.

### 2.3 `MaxFrameBytes` is decorative

`json.Decoder` has no size ceiling — it grows its buffer until OOM. Actually
enforcing the constant requires either `bufio.Scanner` with
`sc.Buffer(nil, MaxFrameBytes)` (per-frame limits, but a 64 KB default that
silently kills large frames if you forget) or length-prefixed framing (check the
header before allocating). With `Decoder`, the real enforcement is
`http.MaxBytesReader` at the HTTP edge, before a frame is ever built. Worth a
comment saying so, or the constant will be trusted later and be wrong.

### 2.4 `Response` needs a `Code` field

With only `Error string`, a handler must string-match to distinguish "duplicate
username" (409) from "bad credentials" (401) — which breaks the moment anyone
rewords a message.

```go
type Response struct {
    ReqId  uint64          `json:"req_id"`
    Status string          `json:"status"`
    Code   string          `json:"code,omitempty"`   // "duplicate_username" — machine contract
    Error  string          `json:"error,omitempty"`  // human-readable detail
    Data   json.RawMessage `json:"data,omitempty"`
}
```

The backend layer then maps codes to sentinel errors once, handlers use
`errors.Is`, and rewording breaks nothing. This also serves §4.10's requirement
that login failures return a *uniform* message: one code for both "no such user"
and "wrong password", while the server still logs the real distinction.

### 2.5 Shutdown does not drain

Closing the listener stops new connections but does nothing to the 8 accepted
ones, whose `handleConn` goroutines are blocked in `ReadRequest` on sockets that
are still open. `s.wg.Wait()` therefore hangs indefinitely. Fix: track accepted
connections and close them on `ctx.Done()`, plus a drain deadline so one wedged
connection cannot hold shutdown hostage.

### 2.6 Accept-error path can spin hot

```go
log.Printf("accept error: %v", err)
continue
```

On a non-shutdown failure — `EMFILE` / "too many open files" is the usual one —
this retries immediately, pinning a core and flooding the logs. `http.Server.Serve`
handles this with exponential backoff: sleep 5 ms, double per consecutive
failure, cap at 1 s, reset on success. About six lines.

### 2.7 Trace ID has no way to cross the process boundary

`protocol.Request.req_id` cannot serve — it is a *multiplexing* correlation ID,
scoped to one connection and reused over time. A separate trace field is needed
on the frame, or in `data`. Worth deciding before `frame.go` is finalized, since
adding a wire field later means touching both sides.

### 2.8 Documentation drift

- `SETUP.md` documents the frontend as `frontend/`; the directory is `web/`.
  `httpserver/server.go`'s own header hedges: *"./web (or ./frontend, depending
  on final path)"*.
- `web/README.md` says the profile page calls `GET /api/user`; `routes.go` and
  `handlers_auth.go` both define `GET /api/me`.
- `web/README.md` lists `default-avatar.png`, and `routes.go` serves
  `GET /default.png`; neither file exists.

---

## Part 3 — Concepts covered

Reference material. Condensed; the detail is in Part 1.

### 3.1 Repo layout

The organizing principle is the two-process split. Every package is **edge**
(`httpserver`), **core** (`tcpserver`), or **shared/infrastructure**.

```
cmd/         one dir per binary; package main, wiring only
internal/    compiler-enforced private — nothing outside this module can import it
web/         static frontend, no build step
scripts/     setup / seed / perf — the three verbs actually run
deploy/      docker-compose; bind-mounts schema.sql into /docker-entrypoint-initdb.d
uploads/     runtime avatar storage, gitignored
perf/        pprof artifacts; .gitignore keeps write-ups, drops raw profiles
```

Dependency direction, with no upward imports:

```
cmd/*  →  httpserver / tcpserver  →  db, cache, crypto, validate, protocol  →  model, config
```

`httpserver` and `tcpserver` never import each other — they agree only via
`protocol`. That is what makes the two-process split real rather than cosmetic,
and why `protocol`, `model`, and `config` are separate packages.

### 3.2 Three pools, not one

| Pool | Location | Size | Custom? | Semantics |
|---|---|---|---|---|
| HTTP → TCP | `httpserver/tcpclient.go` | 8 (`TCPPoolSize`) | yes | **multiplexed** |
| TCP → MySQL | `db/pool.go` | 50 (`MaxOpen`) | yes | **exclusive checkout** |
| TCP → Redis | `cache/redis.go` | 50 (`PoolSize`) | no (go-redis) | library-managed |

The multiplexed/exclusive distinction explains the 8-vs-50 sizing: multiplexed
connections do not scale with concurrency, exclusive ones do. It is also why the
two hand-rolled pools share no code — one needs `pending` maps and a reader
goroutine, the other needs an idle list and a wait queue.

**The buffered-channel-as-semaphore idea** was the right pattern in the wrong
place. It belongs in `db/pool.go`, where it also delivers the wait-with-timeout
requirement for free:

```go
select {
case c := <-p.idle:  return c, nil
case <-ctx.Done():   return nil, ctx.Err()   // the 200ms wait timeout
}
```

### 3.3 Framing: TCP has no messages

TCP guarantees bytes arrive in order, uncorrupted. It does **not** preserve
where one write ended and the next began. Two writes may arrive as one read, or
ten. *Framing is the convention invented to recover message boundaries from a
byte stream.*

| Strategy | Shape |
|---|---|
| Delimiter | read until a byte that cannot appear in the payload |
| Length-prefix | fixed-size header giving body length, then the body |
| Fixed-size | every message exactly N bytes |

Newline is safe for JSON because `encoding/json` never emits a raw newline — a
newline inside a string becomes the two characters `\` and `n`. Hence the "no
pretty-print" rule: `json.Marshal` is safe, `json.MarshalIndent` would corrupt
framing.

**Where this bites later:** JSON cannot hold raw binary, so a 2 MB avatar becomes
~2.7 MB of base64. That 33% inflation is the trigger to switch to length-prefixed
framing, which handles binary natively and makes `MaxFrameBytes` enforceable.

### 3.4 `json.Decoder` delimits structurally and ignores the newline

It parses the JSON *grammar* — tracking brace/bracket depth and string state —
and knows a value is complete when depth returns to zero. Per `Decode` call it
scans its internal buffer, reads more from the conn if incomplete, unmarshals,
advances past the value, and **keeps leftover bytes buffered** for next time.

Two consequences: the decoder routinely **over-reads** into the next frame (so
never mix `dec.Decode()` with direct `conn.Read()`; `dec.Buffered()` returns the
leftovers), and a **malformed frame poisons the connection** — a `*json.SyntaxError`
leaves the decoder parked mid-garbage with no resync, so the only move is to
close.

| | Delimiting | On bad frame |
|---|---|---|
| `json.Decoder` | JSON grammar; newline ignored | connection unrecoverable |
| `bufio.Scanner` + `Unmarshal` | splits on `\n`, then parses | skip the line, continue |

**So why keep the newline?** It is free (`Encode` writes it regardless) and buys
four things: netcat/tcpdump readability, interop with non-Go clients whose JSON
parsers are not streaming, a resync point if recovery is ever wanted, and
capture/replay as a line-oriented file.

### 3.5 Framing gotchas

- **Forgetting `Flush`.** Bytes sit in the `bufio` buffer; the peer waits
  forever. Presents as a network hang, not an error.
- **Concurrent writes interleaving.** Hence the per-connection write mutex.
- **No deadlines.** A dead peer that never sends FIN blocks a read forever.
- **Treating `io.EOF` as an error.** It means a clean close; logging it as a
  failure fills the logs with noise on every normal disconnect.
- **`bufio.Scanner`'s 64 KB token cap.** Returns `bufio.ErrTooLong` past it —
  instant death for a 2.7 MB avatar frame unless `sc.Buffer(...)` raises it.
- **Declare `var req` *inside* the read loop.** Hoisting it and passing `&req`
  to the goroutine means every handler shares one struct the reader keeps
  overwriting — a data race presenting as handlers processing the wrong op.
- **`json.RawMessage` is safe to hand to a goroutine** — `UnmarshalJSON` copies
  (`append(m[0:0], data...)`), so it is not aliasing decoder state.

**Debugging tip:** talk to the TCP server with `nc localhost 9000` and paste
`{"req_id":1,"op":"ping","data":{}}` before writing any client. Fastest possible
loop.

### 3.6 Reader needs no mutex; writer does

| | Relationship | Lock? |
|---|---|---|
| Writes | many-to-one (N goroutines → 1 conn) | yes — `writeMu` |
| Reads | one-to-one (1 goroutine ← 1 conn) | no |

The decoder holds internal buffer state and is not safe for concurrent use, so
the single-reader invariant is what makes the absence of a lock safe. Wanting a
*read* mutex would mean two goroutines reading one connection, which interleaves
**frames**, not just bytes, and is unfixable by locking.

**Important distinction:** "the reader needs no mutex" means no mutex on the
*decoder*. The client-side reader still needs `pendingMu` — that guards the map,
which callers insert into from many goroutines.

**The reader must not do work.** Decode, hand off, go back to reading. If
`handle` ran inline, the connection would process one request at a time and
multiplexing would be dead.

### 3.7 `net/http` middleware

`http.Handler` is an interface with one method; `http.HandlerFunc` is a named
function type with `ServeHTTP` defined on it — an adapter so a plain function
satisfies the interface without declaring a struct.

Middleware is `func(http.Handler) http.Handler`. This is **the** Go standard, not
a workaround for lacking a framework: `chi` takes exactly this signature, and
`chi/middleware` ships `RequestID`, `Logger`, `Recoverer` as drop-ins that work
with a bare `ServeMux`. (`gin`/`echo` invent their own context type and a
different signature; that is where stdlib interop is lost.)

Five active middleware is **lean**, not a lot. Production chains typically also
carry CORS, body limits, timeouts, compression, metrics, tracing, and security
headers.

What bare `net/http` actually costs: pre-written middleware (~60 lines to
replace), routing sugar (largely closed by Go 1.22's method+wildcard
`ServeMux` patterns, which cover this project's whole route table), and the
`ResponseWriter`-wrapping boilerplate for status capture.

### 3.8 Request-scoped state across languages

Go stores it in `context.Context`, passed explicitly. `*http.Request` is treated
as immutable, so you make a shallow copy: `r.WithContext(ctx)`.

**Use an unexported key type**, never a string — context keys compare by value
*and* type, so an unexported type makes collisions with other packages
impossible:

```go
type ctxKey int
const requestIDKey ctxKey = iota
```

| | Mechanism | Style |
|---|---|---|
| Go | `context.Context`, passed explicitly | explicit |
| Java / Spring | `ThreadLocal`, SLF4J `MDC` | ambient |
| Node / Express | mutate `req.x`; or `AsyncLocalStorage` | ambient |
| Python | `contextvars`; Flask's `g` | ambient |
| .NET | `AsyncLocal<T>`, `HttpContext.Items` | ambient |

Go went explicit deliberately: the ambient approaches key off a thread or async
scope, and goroutines are cheap, unnamed, and spawned freely — there is no stable
identity to hang a thread-local on. The cost is verbosity and `any`-typed values;
the benefit is that the value visibly flows into every goroutine, including down
into `db` and `cache` calls that already take `ctx` first.

**Where request IDs come from:** honor an inbound `X-Request-ID` if present (so
traces stitch across services), otherwise generate one — `crypto/random.go`'s
`RandomHex` already exists for this — and echo it back as a response header.

### 3.9 Server lifecycle: an asymmetry

**The HTTP side is easy because the stdlib solved it.** `http.Server.Shutdown(ctx)`
stops accepting, drains in-flight handlers, respects a deadline. Two traps:
`ListenAndServe` returns `http.ErrServerClosed` the instant Shutdown is called
(treating it as a real error makes logs scream on every clean exit), and the
process must actually *wait* for `Shutdown` to return or it dies mid-drain.

**The TCP side is harder because it is hand-rolled.** Closing the listener makes
the blocked `Accept()` return an error that must be distinguished from a real
failure; `wg.Add(1)` must happen in the accept loop *before* the goroutine starts
(inside it, `Wait()` can return before the work registers); draining means
existing connections finish current requests but stop reading new frames; and a
missed deadline must force-close.

**Shutdown order spans both binaries.** Within each: stop accepting → drain
handlers → close downstream pools. Across them: the HTTP server must drain
*before* the TCP server, or in-flight requests reach a backend that has already
closed its MySQL pool. In practice, give the TCP server a longer drain deadline.

`signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`
returns a context that cancels on signal, slotting straight into the `Run(ctx)`
signature both stubs specify. It replaces the old manual `chan os.Signal` dance.

### 3.10 Layering the client: a typed backend over a generic transport

The reader cannot convert responses to typed values — one reader goroutine
serves every op and has no idea whether request 7 was a login or an avatar
update. That knowledge lives with the caller. So the conversion goes at a third
place:

```
readLoop ──chan protocol.Response──▶ Pool.Do ──▶ Backend.Login() ──▶ handler
   (raw)                             (raw)        (typed + errors)    (HTTP)
```

One `call` helper owns everything wire-shaped, so `protocol.Request` appears
exactly once in the whole HTTP tier:

```go
func (b *Backend) call(ctx context.Context, op string, args, out any) error {
    var data json.RawMessage
    if args != nil {
        var err error
        if data, err = json.Marshal(args); err != nil {
            return fmt.Errorf("%s: marshal args: %w", op, err)
        }
    }
    resp, err := b.pool.Do(ctx, protocol.Request{Op: op, Data: data})
    if err != nil { return fmt.Errorf("%s: %w", op, err) }   // transport failure
    if resp.Status == protocol.StatusErr { return backendError(resp) } // application failure
    if out != nil {
        if err := json.Unmarshal(resp.Data, out); err != nil {
            return fmt.Errorf("%s: decode payload: %w", op, err)
        }
    }
    return nil
}

func (b *Backend) Login(ctx context.Context, username, password string) (*LoginResult, error) {
    var out LoginResult
    if err := b.call(ctx, protocol.OpLogin, loginArgs{username, password}, &out); err != nil {
        return nil, err
    }
    return &out, nil
}
```

**Why `any` rather than `json.RawMessage` in the signature.** Putting
`RawMessage` there bakes JSON into the API, so the length-prefixed-binary
stretch goal would require touching every op method. With `any`, that swap is
contained inside `call`. Same reasoning as `database/sql`'s `Query(q, args ...any)`
— serialization is the transport's business. The usual type-safety objection
answers itself: `Login(ctx, username, password string)` is fully typed, and that
is the only surface handlers touch; the `any` is confined to one unexported
function whose callers all live in one file.

**Transport vs application errors are genuinely different** and both must
survive: a transport error (connection died, deadline) is always 500/503; an
application error (`Status: "err"` inside a successful round-trip) maps to a 4xx.

**Payload structs belong in `protocol`.** Their field names *are* the wire
contract — `tcpserver/auth.go` unmarshals the exact bytes `httpserver`
marshalled. Defined twice, they drift, and a rename yields a silent zero value
rather than a compile error. A `protocol/payloads.go` alongside `ops.go` keeps
both sides compiling against one definition. If handlers referencing
`protocol.LoginResult` reads badly, a type alias costs nothing:
`type LoginResult = protocol.LoginResult`.

**On connection death, close the pending channels** rather than leaving waiters
to time out individually. The reader is the only sender, so closing is safe:

```go
select {
case resp, ok := <-ch:
    if !ok { return protocol.Response{}, ErrConnClosed }
case <-ctx.Done():
    c.forget(id)                  // deregister so the map doesn't leak
    return protocol.Response{}, ctx.Err()
}
```

### 3.11 Write-side head-of-line blocking

The one legitimate version of "a connection gets stuck". While a 2.7 MB base64
avatar frame is mid-write on conn 3, everyone else on conn 3 waits on `writeMu`.

**The free-list fix is sound**, and it does not contradict "don't use exclusive
checkout for the mux pool" — the connection is checked out **for the duration of
the write only**, released before the response arrives, so multiplexing is fully
preserved:

```go
free := make(chan *muxConn, 8)   // all 8 seeded at construction
c := <-free                       // a conn nobody is mid-write on
...write...
free <- c                         // back, microseconds later
```

It also replaces `writeMu` entirely — one mechanism instead of two. Costs: ~100 ns
per request versus ~5 ns for `atomic.Add`, and it does not help when all 8 are
genuinely busy.

**The simpler alternative, preferred for now:** segregate large writes. Keep the
8 mux connections for normal ops and dial a separate connection for avatar
uploads. They are rare and already slow; one extra socket costs nothing. Then no
frame on the mux pool exceeds a few hundred bytes and the stall cannot occur.

---

## Part 4 — Decisions taken

| Question | Decision |
|---|---|
| Session validation | Token carried in every op's `data`, resolved TCP-side (option b) |
| Middleware order | `RequestID → Logger → Recover → Session → [RequireAuth]` |
| Handler organization | Stay in `httpserver`; make them methods on a struct, not bare funcs |
| Free-list vs round-robin | KIV — segregate large writes instead if HOL shows up in profiling |
| Reader/writer types | Symmetric two-method types in `protocol`; loops live in each tier |
| `call()` signature | `any` for args and out, so the API is not JSON-bound |
| Payload structs | In `protocol` — they are part of the wire contract |
| Frame reader | `json.Decoder` over `bufio.Scanner` (no size ceiling to remember) |
| Op encoding | String, not int — netcat readability beats a nanosecond of dispatch |

### Build order

Front-loads feedback rather than the hardest deliverable. Diverges from
`plan.md`, which puts the custom pool on Day 4 before the protocol skeleton —
that ordering protects against the pool running long, and is a defensible
alternative. The interface split below means neither choice blocks the other.

| Phase | Build | Why here |
|---|---|---|
| 0 | `config.Load()`; Recover + RequestID + Logger; `protocol/` codec with a single `OpPing`; both mains with listen + SIGTERM | Instrumentation and the wire contract before anything uses them |
| 1 | `tcpclient.go` mux pool; `tcpserver` `conn.go` + `dispatch.go`; `GET /api/ping` end-to-end | **Walking skeleton.** Hardest concurrency code, debugged against a trivial payload |
| 2 | `schema.sql` via docker-compose; `cache/redis.go` + `session.go`; `db/users.go` on stock `database/sql`; seed **10k** rows | Real dependencies, shallow. 10k not 10M — fast iteration |
| 3 | `validate/`, `crypto/`; `handleSignup`/`handleLogin`; cookie helpers; session resolution TCP-side | First real vertical slice — login works from a browser |
| 4 | `db/pool.go` + `conn.go` with prepared-stmt cache; swap behind the interface; `pool_test.go` | A real query workload to exercise health checks and eviction against |
| 5 | `get_me`, nickname, avatar; cache-aside + invalidation; wire up `web/` | Variations on a path that already works |
| 6 | Kick off 10M seed (overnight); load test, pprof, tune | |
| 7 | Rate limiting, CSRF | Explicitly shelved in the stubs |

**The interface trick that makes phase 4 movable.** Define `db/users.go` as an
interface first — the five functions its stub already names. Back it with stock
`database/sql` in phase 2, then swap in the custom pool in phase 4 without
touching a line of business logic. Integration tests for signup/login do not
change; passing before and after the swap demonstrates the pool is correct at the
API level. This decouples the two risks: never blocked on login by pool bugs,
never debugging pool bugs through the login path.

**Hold firm on:** don't let the pool slip past the halfway mark. It is
self-contained and unit-tested, so it can be built in parallel if phase 3 drags.

---

## Part 5 — Open items

### 5.1 The bcrypt arithmetic — settle before Day 9

bcrypt at cost 10 is roughly **60 ms of pure CPU** per verification. At 3000
logins/second that is 180 core-seconds per second — about **180 cores**. Caching
does not help; the cache saves the MySQL lookup, but every login still runs
bcrypt against the stored hash.

So a 3k RPS target made entirely of logins is arithmetically impossible on one
machine at any level of tuning. **The workload mix is therefore the single most
important parameter of the load test.** Realistic traffic is mostly `get_me` and
profile reads with a small fraction of logins, and that shape is achievable.

Decide the mix (something like 90% reads / 10% logins), write it into §8, and
state it alongside the final numbers. A reviewer will ask "3k RPS of *what*", and
a pre-registered answer reads far better than one reverse-justified after the
run.

### 5.2 Carried forward

- **Trace ID across the process boundary** — needs a frame field; decide before
  `frame.go` is finalized (§2.7).
- **Shutdown drain and accept backoff** — §2.5, §2.6.
- **`defer cancel()` discipline** — one leaked context entry per request
  otherwise; enable `go vet`'s `lostcancel` in CI (§1.13).
- **Doc drift** — `frontend/` vs `web/`, `/api/user` vs `/api/me`, the missing
  default avatar (§2.8).
- **TCP keepalive or idle timeouts** — otherwise half-open connections
  accumulate one per client crash (§1.2).

### 5.3 Three phase-5 items that look simple and are not

**`validate/nickname.go`.** `utf8.RuneCountInString` counts runes, but an emoji
family is five runes joined by zero-width joiners, so a "1–32 rune" limit behaves
unintuitively. U+202E (right-to-left override) lets a nickname render as
something else entirely. And `é` can be one rune or two (`e` + combining accent),
so two visually identical nicknames compare unequal without NFC normalization.

**The avatar write path.** Eight ordered steps — `MaxBytesReader` wrapping before
any read, MIME sniffing on the first 512 bytes, the `LAST_INSERT_ID` atomic
version bump, write-to-`.tmp`-then-`rename`, directory sharding, cache
invalidation, orphan cleanup — where getting the order wrong leaves a row
pointing at a file that does not exist. Not concurrency-hard like the pools, but
the most intricate *feature* in the project.

**Cache invalidation ordering.** Delete-on-write is mostly race-safe. The
interleaving that bites: a reader misses, reads the old value from MySQL, then a
writer updates MySQL and deletes the key, then the reader writes its stale value
into the cache — which now holds old data with a full TTL ahead of it. Rare, real,
and the kind of bug that appears once in a demo and cannot be reproduced.

### 5.4 Closing note on difficulty

The technically demanding parts are the two pools, the multiplexing, and perf.
Everything else genuinely is plumbing — but there is a *lot* of it, and plumbing
is where correctness bugs live. "Simple" and "quick" are not the same thing:
phase 5 has more surface area than phase 4 even though phase 4 is harder.
