# Endianness — Recap (+ How Bytes Travel Over the Network)

Context: `internal/sharding/ring.go` uses `binary.LittleEndian.PutUint32/PutUint64`
to turn integers into bytes before hashing them with xxhash. That led to: what is
endianness, why is LE everywhere, and how does "network byte order" fit with
what actually happens between app → kernel → NIC → wire.

For general networking (connection states, TIME_WAIT / CLOSE_WAIT, Nagle,
keepalive, pooling, timeouts) see [`networking.md`](networking.md).

---

## 1. What It Is

Endianness = the order in which the **bytes** of a multi-byte integer are laid
out in memory (or in a message).

| Order | Rule | `0x12345678` as uint32 |
|---|---|---|
| **Big endian** | Most significant byte first (lowest address) | `12 34 56 78` |
| **Little endian** | Least significant byte first (lowest address) | `78 56 34 12` |

Small example — `uint32(2)`:
```
big endian:     00 00 00 02
little endian:  02 00 00 00
```

Endianness only exists when you **interpret several bytes as one number**. A
single byte, or a sequence of independent bytes (text), has no endianness.

---

## 2. Bytes Move, Bits Don't

- Only **whole bytes** are reordered. The contents of each byte are untouched.
  `0x78` stays `0x78` — not `0x87`, not bit-reversed.
- Why: memory is **byte-addressed**. Each address holds one byte; "order" only
  exists between things with separate addresses. Bits inside a byte have no
  address, so software can't observe their order — a byte is always read as
  one value.
- Common slip: thinking `0x02` (`0000 0010`) becomes bit-reversed
  `0100 0000` (`0x40`). It doesn't — the byte `0x02` just moves position.
- Bit order exists only at the physical layer (§7) and is invisible to software.

---

## 3. Who Uses What

There is no single standard — each order has a domain:

| Big endian ("network byte order") | Little endian |
|---|---|
| TCP/IP header fields, most protocols & file formats | x86 / AMD64 |
| HTTP/2 binary frame fields | ARM in practice (Android, iOS, Apple Silicon, Linux servers) |
| Java serialization default | RISC-V |
| IBM mainframes (z/Architecture) | Protobuf fixed-width ints |
| Legacy: Motorola 68k, SPARC, classic PowerPC | |

Rule of thumb: **big endian in protocol headers, little endian in CPU memory.**

---

## 4. Why Little Endian Won CPUs

**History:** Intel 8008/8080 were built to match the Datapoint 2200 terminal,
which processed numbers bit-serially starting from the least significant bit.
x86 inherited it, x86 dominated, and bi-endian ARM settled on LE to match the
ecosystem.

**Technical advantages:**
- **Free narrowing / casts** — the low byte is at the same address for any width.
  Reading a uint64 at address A as uint8/16/32 gives the correctly truncated
  value with no address adjustment.
- **Arithmetic in address order** — addition starts at the least significant byte
  and carries upward, i.e. walks memory upward. Helped early 8-bit CPUs and
  bignum code.

## 5. Where Big Endian Still Wins

- **Human-readable hex dumps** — `00 00 00 25` reads like the number.
- **Byte-wise comparison = numeric order** — `memcmp` on big-endian integers
  sorts them numerically. That's why ordered KV stores (CockroachDB, HBase,
  Bigtable, RocksDB-based systems) encode integer keys big endian; range
  partitioning over raw keys depends on it.
  In LE, 256 (`00 01`) would sort before 1 (`01 00`).
- **Convention** — RFCs standardised it for protocol headers early (IETF era
  machines were largely BE), so "network byte order" = BE stuck.

---

## 6. In `ring.go`

```go
var buf [12]byte
binary.LittleEndian.PutUint32(buf[0:4], uint32(s))   // bytes 0–3:  shard
binary.LittleEndian.PutUint64(buf[4:12], uint64(v))  // bytes 4–11: vnode index
h := xxhash.Sum64(buf[:])
```

Shard 2, vnode 37:
```
index:  0  1  2  3 | 4  5  6  7  8  9  10 11
bytes:  02 00 00 00| 25 00 00 00 00 00 00 00
        └ shard=2 ┘ └────── vnode=37 ─────┘
```

**Why encode at all:** xxhash hashes **bytes**, not integers.

**Why it matters:**
1. **Determinism across machines** — explicit byte order gives identical bytes
   (→ identical ring) on any CPU. Copying raw memory would use native order, and
   a big-endian box would build a different ring.
2. **Arbitrary but permanent** — BE would work equally well, but once data is
   placed, switching order changes every hash → nearly every key moves. The
   encoding is part of the sharding contract, like the hash function and vnode
   count.
3. **Fixed widths prevent ambiguity** — 4 bytes for shard, 8 for vnode, always.
   Different (shard, vnode) pairs can never produce the same bytes. Contrast
   string concatenation: `"1"+"23"` == `"12"+"3"` == `"123"`.
4. **Other tools must match exactly** — a Python migration/verification script
   needs `struct.pack('<Q', user_id)` (`<` = little endian, `Q` = uint64) then
   xxhash64 seed 0, or it computes the wrong shard.

**Why LE here specifically:** the bytes only feed a hash — nobody reads or sorts
them, so readability and sort order are irrelevant; only speed matters. LE
matches the CPU, so `PutUint64` compiles to a single store with no byte swap.
For bytes going on the wire or into a sorted key → use `binary.BigEndian`.

`ShardForString` has no endianness concern: `Sum64String` hashes the string's
UTF-8 bytes directly, and a string has only one byte order.

---

# Part II — Endianness on the Network

---

## 7. Two Different Orders, Two Different Layers

| Layer | Unit | Order rule | Who defines it | Who deals with it |
|---|---|---|---|---|
| Protocol headers (IP, TCP, UDP, DNS, HTTP/2 frames) | Bytes | **Big endian** — MSB byte first | RFCs / specs | Kernel (and you, for your own protocol) |
| Your app payload | Bytes | **Whatever your protocol chose** | You | You |
| Physical transmission | Bits / symbols | Bit order within each byte (Ethernet: LSB first) | Link standard (IEEE 802.3, Wi-Fi…) | NIC hardware only |

- **"Network byte order"** means the byte order of multi-byte fields *inside
  protocol messages*. It is about bytes, not bits.
- Protocols like TCP/IP are **defined in octets** ("dest port = 16-bit field at
  bytes 2–3, MSB first"). They never mention wire bits, because TCP must run
  over Ethernet, Wi-Fi, fibre, 5G — each turns bytes into signals differently.
- The two orders are independent: **byte order is fixed when the header is
  written into memory; bit order is applied when each byte leaves the NIC.**

---

## 8. Encapsulation — What a Frame Looks Like

Each layer **prepends** its header to what the layer above gave it (Ethernet
also appends a trailer):

```
┌──────────┬────────┬─────────┬─────────────────────────┬─────┐
│ Ethernet │ IP     │ TCP     │ payload                 │ FCS │
│ header   │ header │ header  │ (your app's bytes)      │     │
└──────────┴────────┴─────────┴─────────────────────────┴─────┘
```

| Header | Key fields | Purpose |
|---|---|---|
| Ethernet | Dest/src MAC, EtherType | Deliver to the **next device on the local link** (hop by hop) |
| IP | Src/dst IP, TTL, protocol, header checksum | **Route** across networks, end to end |
| TCP | Src/dst port, seq #, ack #, flags, window, checksum | Which **app** (port); **ordering**, retransmission, de-dup (seq/ack); flow control (window); integrity |
| FCS (trailer) | CRC-32 | Detect corruption on this link |

All integer fields in Ethernet/IP/TCP headers are **big endian**.

**HTTP is not a network header.** HTTP lives *inside* the TCP payload — it's
application data:
- HTTP/1.1 headers are text → no endianness.
- HTTP/2 / HTTP/3 frames are binary → their integer fields are BE (per spec).

---

## 9. The Send Path (App → Wire)

```
your app        write(sock, buf)            payload bytes in user memory
   │                                        (byte order = whatever app wrote)
   ▼  syscall: copy into kernel
kernel: socket  send buffer                 bytes queued; TCP decides segment sizes
   │
kernel: TCP     prepend TCP header          ports, seq/ack, window — BE fields
   │            (+ checksum over header+payload, or offloaded to NIC)
kernel: IP      prepend IP header           IPs, TTL — BE fields; route lookup
   │
kernel: L2      prepend Ethernet header     MACs (ARP lookup for next hop)
   │
driver          put a descriptor in the     descriptor = "frame is at RAM addr X, len Y"
   │            NIC's TX ring buffer
   ▼
NIC             DMA: reads frame from RAM   no CPU involvement in the copy
   │            computes FCS (CRC)
   ▼
PHY             bytes → bits/symbols        byte 0 first; within each byte LSB first;
   │            (encoded, e.g. 64b/66b)     scrambled & encoded at gigabit+
   ▼
wire / fibre / radio
```

Key points:
- **The whole frame is built in RAM before transmission starts.** Checksums
  need all the data (TCP checksum in the header covers the payload; FCS covers
  the whole frame), and layers hand off whole packet buffers, not byte streams.
- **DMA (Direct Memory Access):** the NIC reads/writes RAM itself; the CPU just
  posts descriptors into a **ring buffer** (a circular queue shared with the NIC).
- **Offloads:** modern NICs often compute TCP/IP checksums (checksum offload) and
  even split one big buffer into MTU-sized segments (TSO — TCP Segmentation
  Offload), saving CPU.

---

## 10. The Receive Path (Wire → App)

```
wire
   ▼
PHY             signal → bits → bytes
   ▼
NIC             checks FCS (drops bad frames), checks dest MAC,
   │            DMA: writes frame into a pre-posted RX buffer in RAM
   │            raises an interrupt (or is polled — Linux NAPI under load)
   ▼
driver          hands frame buffer to kernel network stack
   ▼
kernel: L2      strip Ethernet header (EtherType says "IPv4/IPv6")
kernel: IP      verify header, dst IP is us; strip IP header (protocol = TCP)
kernel: TCP     verify checksum; find socket by (src IP, src port, dst IP, dst port);
   │            use seq # to order segments, drop duplicates, buffer gaps;
   │            send ACKs; strip TCP header
   ▼
kernel: socket  append payload bytes to receive buffer, in order
   ▼  syscall: copy to user memory
your app        read(sock, buf)             the exact bytes the sender wrote
                                            → interpret per your protocol
```

- When the kernel reads a BE header field (e.g. port) on an LE CPU, it swaps on
  load (`ntohs` / a single `bswap`/`rev` instruction) — essentially free.
- **GRO (Generic Receive Offload)** may merge consecutive segments before TCP
  processing, the receive-side mirror of TSO.

---

## 11. Why There's No Inefficiency from BE Headers + LSB-First Bits

Tempting worry: "TCP header is BE (MSB byte first) but Ethernet sends LSB first —
doesn't Ethernet have to wait for a whole field/header before it can start?"

No, for three reasons:
1. **Ethernet never reorders bytes.** It sends byte 0, byte 1, … in memory order.
   The "LSB first" rule applies only to the 8 bits *within* each byte, and a byte
   is always fully available.
   ```
   port 443 = 0x01BB → memory: [k]=01 [k+1]=BB   (BE, written by kernel)
   wire:  0x01 → 1 0 0 0 0 0 0 0,  then 0xBB → 1 1 0 1 1 1 0 1
   ```
   The MSB byte goes first, exactly as the BE header laid it out.
2. **There's no streaming handoff anyway.** The full frame is in RAM (§9) before
   the first bit goes out.
3. **Bit order costs nothing in hardware.** A shift register loads 8 bits in
   parallel and shifts them out; LSB-first vs MSB-first is just which end is
   wired to the output. At gigabit+, multiple bits per symbol go out in
   parallel over several pairs — "bit order" is a PHY-internal detail.

So Ethernet is effectively **big-endian bytes (as given), LSB-first bits**, and
the two never conflict.

---

## 12. Your Payload Has No Inherent Endianness

Misconception: "data is stored in little endian, wrapped in a big-endian TCP
header, and arrives still in little endian for use."

Reality:
- LE is how the **CPU stores integers in its own memory**. The payload is just
  the bytes your app handed to `write()`.
- **Text** (HTTP/1.1, JSON, usernames): no endianness. `"alice"` =
  `61 6C 69 63 65` on every machine.
- **Binary integers** in your payload: whatever order your protocol chose.
  `binary.BigEndian.PutUint32` → BE on the wire. Dumping a raw in-memory struct →
  LE (native) on the wire, and the receiver had better know.
- TCP and Ethernet never touch payload byte order. The receiver gets **exactly
  the bytes the sender wrote** and interprets them per the agreed format.

| Thing | Byte order |
|---|---|
| Integers in CPU memory (x86/ARM) | Little endian |
| Ethernet / IP / TCP header fields | Big endian (RFCs) |
| Bits within each byte on Ethernet | LSB first (hardware only) |
| Your payload — text | None |
| Your payload — binary integers | Whatever your protocol chose |

---

## 13. TCP Is a Byte Stream → Length Prefixes

- TCP preserves **byte order of the stream**, not **message boundaries**. Two
  `write()`s may arrive as one `read()`, or split across three.
- So binary protocols frame messages, typically with a **length prefix**:
  ```
  [ 4-byte length ][ message bytes ... ][ 4-byte length ][ message bytes ... ]
  ```
- That length prefix is a multi-byte integer → **this is where you choose an
  endianness**, and both sides must agree:
  ```go
  // sender
  binary.BigEndian.PutUint32(hdr[:], uint32(len(msg)))
  conn.Write(hdr[:]); conn.Write(msg)

  // receiver
  io.ReadFull(conn, hdr[:])
  n := binary.BigEndian.Uint32(hdr[:])
  io.ReadFull(conn, msg[:n])          // ReadFull: loop until n bytes arrive
  ```
- BE is the conventional choice for wire formats (matches network byte order,
  readable in packet captures); LE is fine if both ends agree.

---

## 14. Where You Actually Touch Byte Order as a Programmer

1. **Protocol headers** — kernel + socket APIs handle it (`htons`/`ntohs` in C;
   Go's `net` package does it for you when you pass a port).
2. **Your own binary formats** — length prefixes, binary message fields, file
   formats, sortable DB keys. Your choice; be explicit and consistent.
3. **Hash inputs / content addressing** (like `ring.go`) — must be explicit so
   every machine and tool produces the same bytes.

You never deal with bit order on the wire.

---

## 15. Trivia

The name comes from *Gulliver's Travels* (nations at war over which end of an
egg to crack), borrowed by Danny Cohen's 1980 paper *"On Holy Wars and a Plea
for Peace"* — the point being the choice is mostly arbitrary.

---

## 16. Quick Self-Check

**Endianness basics**
1. Write `0x0000ABCD` as uint32 in LE and BE.
   → LE `CD AB 00 00`, BE `00 00 AB CD`
2. Does LE turn the byte `0xAB` into `0xBA`? → No; bytes move whole.
3. Why do sorted KV stores use BE for integer keys? → Byte-wise compare = numeric order.
4. Why is LE free on x86/ARM? → Native order; no swap needed.
5. What breaks if `ring.go` switched to BigEndian? → Every vnode and key hash
   changes → nearly all keys reroute → full data migration.

**Networking**
6. "Network byte order" — bytes or bits? → Bytes: order of multi-byte header fields.
7. Who decides bit order on the wire? → The link standard; NIC hardware applies it.
8. Is an HTTP header a network-layer header? → No; it's app data in the TCP payload.
9. Which header carries sequence numbers, and what for? → TCP; ordering,
   retransmission, de-duplication, ACKs.
10. Does Ethernet wait for a full BE field before sending? → No; it sends bytes in
    order, only bits within a byte are LSB-first, and the frame is already in RAM.
11. What does the NIC use to read the frame from RAM? → DMA, via descriptors in a
    TX ring buffer.
12. Is the payload "in little endian"? → No inherent order; text has none, binary
    ints use whatever the app's protocol chose.
13. Why do binary protocols need a length prefix over TCP? → TCP is a byte
    stream with no message boundaries.
