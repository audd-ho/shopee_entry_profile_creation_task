# Networking — Recap (TCP, Connection States, Tuning)

General networking concepts, framework-agnostic, focused on how connections
behave. A short project-specific section is at the end (§17). For a deeper
dive into byte order and the byte-level packet path, see
[`endianness.md`](endianness.md) Part II (optional).

---

## 1. Layer Model

| TCP/IP layer | OSI rough equivalent | Unit | Examples | Addressing |
|---|---|---|---|---|
| Application | 5–7 | message / stream | HTTP, DNS, MySQL protocol, Redis RESP, your own protocol | — |
| Transport | 4 | segment (TCP) / datagram (UDP) | TCP, UDP, QUIC (over UDP) | **port** |
| Internet | 3 | packet | IPv4, IPv6, ICMP | **IP address** |
| Link | 2 | frame | Ethernet, Wi-Fi | **MAC address** |
| Physical | 1 | bits / symbols | copper, fibre, radio | — |

- Each layer **prepends** its header (encapsulation); receiver strips them in
  reverse:
  ```
  ┌──────────┬────────┬─────────┬──────────────────────┬─────┐
  │ Ethernet │ IP     │ TCP     │ payload (app bytes,  │ FCS │
  │ (MACs)   │ (IPs)  │ (ports, │ e.g. HTTP request)   │(CRC)│
  │          │        │ seq/ack)│                      │     │
  └──────────┴────────┴─────────┴──────────────────────┴─────┘
  ```
- **Send path:** app `write()` → kernel socket send buffer → TCP/IP/Ethernet
  headers added → driver posts the frame to the NIC → NIC reads it from RAM
  (DMA) → bits on the wire.
  **Receive path:** NIC → frame DMA'd into RAM → kernel strips headers, TCP
  reorders by sequence number → socket receive buffer → app `read()`.
- HTTP headers are **not** network headers — they're application data inside
  the TCP payload.
- IP is **end to end** (source → final destination); Ethernet is **hop by hop**
  (MAC addresses are rewritten at every router).
- **TLS** sits between TCP and the application (encrypts the byte stream).

---

## 2. Addressing a Connection

- A TCP connection is uniquely identified by its **4-tuple**:
  `(src IP, src port, dst IP, dst port)`.
- Server listens on a fixed port (e.g. 8080). Client gets an **ephemeral port**
  from the OS (Linux default range `32768–60999` ≈ 28k ports; see
  `net.ipv4.ip_local_port_range`).
- Consequence: one client IP → one server IP:port can have at most ~28k
  simultaneous connections (or connections lingering in TIME_WAIT, §6). A server
  can hold far more, since each client brings its own IP/port.
- Each connection is a **file descriptor** in the process → bounded by
  `ulimit -n`.

---

## 3. TCP vs UDP

| | TCP | UDP |
|---|---|---|
| Connection | Yes (handshake, state) | No |
| Delivery | Reliable, ordered, de-duplicated | Best effort; may drop, reorder, duplicate |
| Boundaries | **Byte stream** — no message boundaries | **Datagrams** — boundaries preserved |
| Flow / congestion control | Yes | No (app's job) |
| Use | HTTP/1.1, HTTP/2, DBs, most RPC | DNS, video/voice, games, QUIC/HTTP/3 |

QUIC = reliable, multiplexed, encrypted transport built on UDP, to escape TCP's
head-of-line blocking (§11) and kernel-level ossification.

---

## 4. Connection Setup — 3-Way Handshake

```
client                                 server (LISTEN)
  │ ── SYN (seq=x) ─────────────────▶ │   SYN_RCVD  (in SYN queue)
  │ ◀── SYN-ACK (seq=y, ack=x+1) ──── │
  │ ── ACK (ack=y+1) ───────────────▶ │   ESTABLISHED (moved to accept queue)
ESTABLISHED                            │   app calls accept() → gets the conn
```

- Costs **1 RTT** before any data (TLS adds 1–2 more RTTs; TLS 1.3 = 1).
  → main reason to reuse connections (§13).
- **Two kernel queues per listening socket:**
  - **SYN queue** — half-open connections (got SYN, waiting for ACK).
    SYN flood attacks fill it; mitigated with SYN cookies.
  - **Accept queue** — fully established, waiting for the app to `accept()`.
    Size = `min(backlog arg to listen(), net.core.somaxconn)` (Linux default
    somaxconn 4096 since 5.4, 128 before). If the app accepts too slowly, it
    overflows → new connections dropped/time out.
- **Sequence numbers** count **bytes**, not packets. Initial values are random
  (security). ACK `n` = "I've received every byte before n".

---

## 5. Connection Teardown — 4-Way Close & States

Each direction is closed independently (FIN = "I'm done sending").

```
active closer (calls close() first)        passive closer
ESTABLISHED                                 ESTABLISHED
  │ ── FIN ───────────────────────────────▶ │
FIN_WAIT_1                                  CLOSE_WAIT   ← app must now call close()
  │ ◀── ACK ─────────────────────────────── │
FIN_WAIT_2                                  │ (app may still send data)
  │                                         │ app calls close()
  │ ◀── FIN ─────────────────────────────── │ LAST_ACK
TIME_WAIT                                   │
  │ ── ACK ───────────────────────────────▶ │ CLOSED
  │ (wait 2×MSL)
CLOSED
```

| State | Side | Meaning | Healthy? |
|---|---|---|---|
| `FIN_WAIT_1` | active | Sent FIN, awaiting ACK | Brief |
| `FIN_WAIT_2` | active | Our side closed; waiting for peer's FIN | Brief; long = peer stuck in CLOSE_WAIT |
| `CLOSE_WAIT` | passive | Peer closed; **waiting for our app to close()** | Brief; piles up = **our bug** |
| `LAST_ACK` | passive | Sent our FIN, awaiting final ACK | Brief |
| `TIME_WAIT` | active | Fully closed; lingering to absorb stragglers | Normal, but can pile up |

**Half-close:** `shutdown(SHUT_WR)` sends FIN but keeps reading — "I'm done
sending, still listening for your reply."

**RST (reset)** — abortive close, no handshake, no TIME_WAIT:
- Sent when a segment arrives for a port with no listener → "connection refused".
- Sent when closing with unread data in the receive buffer, or with
  `SO_LINGER {on, 0}`.
- App sees "connection reset by peer".

---

## 6. TIME_WAIT

**Who:** the side that **closes first** (active closer).

**How long:** 2 × MSL (Maximum Segment Lifetime). RFC suggests MSL = 2 min;
**Linux hard-codes TIME_WAIT at 60 s**.

**Why it exists:**
1. **Reliable final ACK** — if the last ACK is lost, the peer resends FIN; the
   TIME_WAIT side is still there to re-ACK (instead of answering with RST).
2. **Stale segments** — delayed packets from the old connection must expire
   before the same 4-tuple is reused, or they could be mistaken for data in a
   new connection.

**When it hurts:**
- A client opening **many short-lived connections** to the same server
  (no keep-alive, load generators, naive HTTP clients). Each closed connection
  holds its 4-tuple for 60 s → ~28k ports / 60 s ≈ **~470 new conns/s** per
  destination before **ephemeral port exhaustion** ("cannot assign requested
  address").
- Servers rarely suffer: their side of the tuple is fixed and TIME_WAIT costs
  only a small kernel struct, not a file descriptor.

**Fixes (best first):**
1. **Reuse connections** — keep-alive / connection pools (§13). Eliminates the
   problem at the source.
2. **`net.ipv4.tcp_tw_reuse = 1`** — lets *outgoing* connections reuse a
   TIME_WAIT tuple when safe (uses TCP timestamps).
3. More source IPs / wider ephemeral range.
4. **Never** `tcp_tw_recycle` — broke clients behind NAT; removed in Linux 4.12.

**Related — `SO_REUSEADDR`:** lets a restarted server `bind()` its port while
old connections on it are in TIME_WAIT (else "address already in use"). Go sets
it by default on listeners.
**`SO_REUSEPORT`** is different: multiple sockets/processes listen on the same
port with kernel load-balancing.

---

## 7. CLOSE_WAIT

**Who:** the side that **received** the FIN (passive closer).

**Meaning:** "Peer is done; the kernel is waiting for **my application** to call
`close()`." The kernel cannot leave this state on its own.

**Piling up = application bug (resource leak):**
- Code reads, gets EOF (`read` returns 0 / `io.EOF`), but never closes the socket.
- Error paths that `return` without `close`.
- Pooled connections the pool never checks — peer closed idle conn, pool keeps it.

**Consequences:** leaked file descriptors → eventually "too many open files";
the peer sits in FIN_WAIT_2.

**Fix:** always close on EOF and on every error path (Go: `defer conn.Close()`
right after accept/dial). Pools should validate or expire idle connections.

**Diagnose:**
```sh
ss -tan state close-wait          # Linux
ss -tan | awk '{print $1}' | sort | uniq -c   # count by state
netstat -an | grep CLOSE_WAIT     # macOS
lsof -i -P | grep CLOSE_WAIT      # which process owns them
```

Mnemonic: **TIME_WAIT = normal side effect of closing first; CLOSE_WAIT = you
forgot to close.**

---

## 8. Flow Control vs Congestion Control

| | Flow control | Congestion control |
|---|---|---|
| Protects | The **receiver** | The **network** |
| Mechanism | Receive window (`rwnd`) advertised in every ACK: "I have this much buffer free" | Congestion window (`cwnd`) kept by the sender, inferred from loss/delay |
| Effect | Sender never has more than `min(rwnd, cwnd)` unacked bytes in flight |

- **Zero window:** slow reader → receive buffer full → window 0 → sender stops.
  This is TCP-level **backpressure**; a blocked `write()` in your app is often this.
- **Slow start:** new connection starts with small `cwnd` (~10 segments) and
  doubles per RTT until loss or threshold → new connections are slow at first
  (another reason to reuse them).
- **Algorithms:** Reno (AIMD), **CUBIC** (Linux default), **BBR** (Google,
  models bandwidth + RTT instead of reacting to loss).
- **Bandwidth-delay product** = bandwidth × RTT = bytes needed in flight to fill
  the pipe. Windows/buffers smaller than BDP cap throughput.

---

## 9. Nagle's Algorithm & TCP_NODELAY

**Nagle (RFC 896):** if there's unacknowledged data in flight, **buffer small
writes** and send them together when the ACK arrives (or a full MSS
accumulates). Goal: avoid "tinygrams" (e.g. 1-byte keystrokes with 40 bytes of
headers each).

**Delayed ACK** (receiver side): wait briefly (Linux ~40 ms, some OSes 200 ms)
to piggyback the ACK on a response or ACK two segments at once.

**The bad interaction — write-write-read:**
```
client: write(header)   → sent immediately (nothing in flight)
client: write(body)     → Nagle holds it: waiting for ACK of header
server: gets header, can't respond until body arrives, delays ACK (~40 ms)
→ ~40 ms stall per request, for no reason
```

**`TCP_NODELAY`** disables Nagle → every write is sent immediately.
- Standard for latency-sensitive request/response protocols (RPC, DBs, Redis).
- **Go enables `TCP_NODELAY` by default** on all TCP connections
  (`conn.SetNoDelay(false)` to turn Nagle back on).

**Better than relying on either:** build the whole message in user space and do
**one `write()` per message** (e.g. `bufio.Writer` + `Flush()`). Then Nagle has
nothing to coalesce and NODELAY doesn't spray tiny packets.

**`TCP_CORK`** (Linux): the opposite — hold everything until uncorked or a full
segment; used for e.g. header + `sendfile()` body.

---

## 10. Keepalive — Three Different Things

| Kind | Layer | What it does | Why |
|---|---|---|---|
| **TCP keepalive** (`SO_KEEPALIVE`) | Kernel | After idle time, sends empty probes; no reply after N probes → connection errors out | Detect dead peers (crashed host, pulled cable) on idle connections; keep NAT/LB state alive |
| **HTTP keep-alive** (persistent connections) | Application | Reuse one TCP connection for many HTTP requests (default in HTTP/1.1) | Avoid handshake + TIME_WAIT per request |
| **App heartbeats** (ping/pong) | Application | Protocol-level messages (WebSocket ping, gRPC keepalive, Redis PING) | Detect *hung* peers — process alive but not responding — which TCP keepalive can't see |

**TCP keepalive defaults:**
- Linux: idle **2 hours**, interval 75 s, 9 probes — far too slow for most services.
- **Go: enabled by default with 15 s** idle/interval on both dialed and accepted
  connections (tunable via `net.Dialer.KeepAlive` / `KeepAliveConfig` in Go 1.23+).

**Middleboxes:** NATs, firewalls and load balancers drop idle connection state
after some minutes; the next packet then gets RST or vanishes. Keepalive (or
heartbeats) shorter than their idle timeout keeps the entry alive.

---

## 11. Head-of-Line (HOL) Blocking

- **TCP level:** delivery is strictly in order. One lost segment stalls every
  byte behind it until retransmitted — even bytes belonging to unrelated
  requests multiplexed on the same connection.
- **Application level:** with multiplexing over one connection, one huge
  message (e.g. a multi-MB upload) occupies the stream while small messages
  queue behind it.
- **History:** HTTP/1.1 → one request at a time per connection (browsers open ~6).
  HTTP/2 → multiplexes streams over one TCP conn (fixes app-level HOL, not TCP's).
  HTTP/3/QUIC → independent streams over UDP (fixes both).
- **Mitigations:** several connections instead of one; chunk large messages;
  separate connections for bulk vs latency-sensitive traffic.

---

## 12. Framing on a Byte Stream

TCP preserves **byte order**, not **message boundaries**: two `write()`s may
arrive in one `read()`, or one write across several reads. Every protocol must
frame messages:

| Method | Example | Pros | Cons |
|---|---|---|---|
| **Delimiter** | `\n`-terminated (NDJSON, Redis inline, SMTP), `\r\n\r\n` (HTTP headers) | Simple, human-readable, easy to debug with `nc` | Delimiter must be escaped / impossible in payload; must scan for it; one corrupt frame can desync the stream |
| **Length prefix** | 4-byte length + body (gRPC, Kafka, MySQL protocol, Postgres) | Read exact sizes, no scanning, binary-safe, easy max-size check | Not human-readable; the length is a multi-byte integer, so both sides must agree on byte order (big endian is the wire convention) |
| **Self-describing** | Content-Length / chunked encoding (HTTP bodies) | Flexible | More parsing logic |

Always loop until you have a full frame (`io.ReadFull`, buffered readers) and
enforce a **max frame size** to avoid memory blow-ups from bad or malicious input.

---

## 13. Connection Pooling & Reuse

**Why reuse:**
1. Skip handshake RTT (+ TLS handshake).
2. Skip slow start — warmed-up `cwnd`.
3. No TIME_WAIT churn / ephemeral port exhaustion.
4. Bounded resource use on the server (DB connections are expensive: a MySQL
   connection = a server thread + memory).

**Knobs (names vary by library):**
| Knob | Purpose |
|---|---|
| Max open | Cap total connections (protects the server; `max_connections` on MySQL) |
| Max/min idle | Keep warm connections ready; min idle avoids handshakes on the critical path |
| Max lifetime | Recycle periodically — must be **shorter** than server/LB idle timeouts (MySQL `wait_timeout`, LB idle timeout), else you reuse a conn the other side already closed |
| Idle timeout | Close connections unused for a while |
| Health check | Validate (ping) before use or on a timer |

**Sizing rule of thumb:** total connections hitting a server =
`instances × pools per instance × max open per pool`. Check it against the
server's limit. Bigger isn't faster: past the server's core count, extra DB
connections mostly add contention.

**Multiplexing vs pooling:**
- **Pool (one request per conn at a time):** simple; concurrency = pool size.
  database/sql, go-redis, HTTP/1.1.
- **Multiplexed (many in-flight requests per conn, matched by request ID):**
  fewer connections; needs a reader goroutine + ID → waiter map; subject to
  app-level HOL (§11). HTTP/2, gRPC.

---

## 14. Timeouts

| Timeout | Guards against |
|---|---|
| **Connect / dial** | Unreachable host, dropped SYNs (Linux SYN retries take ~2 min by default!) |
| **Read** | Peer stops sending mid-response |
| **Write** | Peer stops reading → window 0 → write blocks forever |
| **Idle** | Holding resources for clients that went quiet |
| **Overall request deadline** | Sum of everything; what the caller actually cares about |

- Go: `conn.SetDeadline/SetReadDeadline/SetWriteDeadline` are **absolute times**
  — reset them per operation. `context` deadlines for the request as a whole.
- **Nest timeouts outward:** inner (DB, lock wait) < middle (backend call) <
  outer (HTTP write timeout). Otherwise the outer layer gives up while inner
  work keeps running, or the client gets a cut-off response instead of a clean error.
- Without timeouts, a single dead peer can pin goroutines, connections and
  memory indefinitely.

---

## 15. Other Useful Facts

- **MTU / MSS:** Ethernet MTU = 1500 bytes per frame; minus 20 B IP + 20 B TCP →
  **MSS ≈ 1460 B** of payload per segment. Jumbo frames: 9000. Path MTU
  discovery avoids IP fragmentation.
- **Socket buffers:** each socket has kernel send/receive buffers
  (`SO_SNDBUF`/`SO_RCVBUF`, autotuned on Linux). `write()` returns once data is
  in the send buffer — **not** when the peer received it.
- **Loopback** (`127.0.0.1`) still runs the full TCP stack (handshakes, TIME_WAIT,
  ports) — just no NIC. Unix domain sockets skip TCP/IP entirely for same-host IPC.
- **Kernel limits to remember:** `ulimit -n` (fds), `net.core.somaxconn`
  (accept queue), `net.ipv4.ip_local_port_range` (ephemeral ports),
  `nf_conntrack_max` (if a firewall/NAT tracks connections).
- **Tools:** `ss` / `netstat` (sockets & states), `lsof -i` (which process),
  `tcpdump` / Wireshark (packets), `curl -v`, `nc` (raw TCP), `dig` (DNS),
  `mtr` / `traceroute` (path). macOS lacks `ss` → `netstat -an`, `lsof`.

---

## 16. Quick Self-Check

1. What identifies a TCP connection? → The 4-tuple (src IP, src port, dst IP, dst port).
2. Which side enters TIME_WAIT, and for how long on Linux? → Active closer; 60 s.
3. Two reasons TIME_WAIT exists? → Re-ACK a retransmitted FIN; let stale
   segments expire before the tuple is reused.
4. Many CLOSE_WAIT sockets — whose fault? → Ours: app received FIN but never closed.
5. Ephemeral port exhaustion — typical cause and best fix? → Many short-lived
   outbound conns → reuse connections (keep-alive/pools).
6. What does Nagle do, and what's the classic stall? → Buffers small writes while
   data is unacked; write-write-read + delayed ACK → ~40 ms stalls.
7. Does Go use Nagle by default? → No, `TCP_NODELAY` is on by default.
8. TCP keepalive vs HTTP keep-alive? → Kernel dead-peer probes vs reusing a
   connection for many requests.
9. What can app heartbeats detect that TCP keepalive can't? → A hung process
   whose kernel still ACKs.
10. Flow vs congestion control? → Protect receiver (rwnd) vs protect network (cwnd).
11. Why must pool max-lifetime be below the server's idle timeout? → Otherwise the
    pool hands out conns the server already closed.
12. Delimiter vs length-prefix framing trade-off? → Readable/simple vs
    binary-safe/no scanning/easy size limits.
13. Does `write()` returning mean the peer got the data? → No — only that it's in
    the local kernel send buffer.

---

## 17. Relation to This Project

| Concept | Where it shows up |
|---|---|
| **Multiplexed persistent connections (§13)** | HTTP server → TCP server uses a pool of persistent connections, each carrying many in-flight requests matched by `req_id` (`internal/httpserver/tcpclient.go`): one reader goroutine per conn routes responses to waiting channels. |
| **Framing (§12)** | Wire protocol is **newline-delimited JSON** (`internal/protocol/frame.go`). Text → no endianness. The comments in that file note the delimiter trade-off firsthand: a malformed frame leaves the decoder stuck, since a byte stream has no boundaries to resync on. Length-prefixed binary framing is listed as a stretch goal. |
| **Nagle / one write per message (§9)** | `ConnectionFrameWriter` encodes into a `bufio.Writer` and `Flush()`es once per message → one write per frame. Combined with Go's default `TCP_NODELAY`, no Nagle stalls. |
| **TIME_WAIT avoidance (§6)** | Persistent HTTP→TCP, Redis and MySQL pools mean no per-request connect/close. The load generator side matters too: `loadtest` uses keep-alive connections (`-connections` flag) — without them a load test would hit ephemeral port exhaustion long before the server's limits. |
| **CLOSE_WAIT avoidance (§7)** | The TCP server's connection handler checks for `io.EOF` and closes the connection (`internal/tcpserver/conn.go`) — exactly the close-on-EOF discipline that prevents CLOSE_WAIT leaks. |
| **Keepalive (§10)** | No explicit settings → Go's default 15 s TCP keepalive applies to both the dialed (HTTP side) and accepted (TCP server side) connections. |
| **Min idle connections (§13)** | Redis client keeps `MinIdleConns` warm so requests don't pay TCP setup on the critical path (`internal/cache/redis.go`). |
| **Connection budget (§13)** | Sharded MySQL → one pool per shard per process. Total MySQL connections = processes × shards × max open per pool; must fit under each MySQL instance's `max_connections`. |
| **Timeout nesting (§14)** | Config validation enforces HTTP write timeout > backend timeout, and the TCP server refuses to start if lock-stack / request / MySQL lock-wait timeouts don't nest (`checkWriteTimeouts` in `cmd/tcp-server/main.go`). |
| **App-level HOL (§11)** | A large avatar upload frame (~2 MB) on a multiplexed connection delays the other responses sharing that connection; more connections in the pool spread this out. |
