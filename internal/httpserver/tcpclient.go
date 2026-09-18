package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"entry_profile_creation/internal/protocol"
)

// TCPClient is a pool of persistent multiplexed TCP connections to the TCP server.
// Requests are correlated to responses via req_id.
type TCPClient struct {
	addr  string
	conns []*muxConn
	rr    atomic.Uint64 // round-robin index
	mu    sync.Mutex
}

// muxConn is one persistent TCP connection with an in-flight request map.
type muxConn struct {
	c         net.Conn
	addr      string
	writeMu   sync.Mutex
	nextID    atomic.Uint64
	pending   map[uint64]chan *protocol.Response
	pendingMu sync.Mutex
	closed    atomic.Bool
	reconnect chan struct{}
}

// NewTCPClient dials `size` connections to `addr`.
func NewTCPClient(addr string, size int) (*TCPClient, error) {
	tc := &TCPClient{addr: addr}
	for i := 0; i < size; i++ {
		mc, err := newMuxConn(addr)
		if err != nil {
			// Close any already-open conns
			_ = tc.Close()
			return nil, fmt.Errorf("dial tcp backend: %w", err)
		}
		tc.conns = append(tc.conns, mc)
	}
	return tc, nil
}

func newMuxConn(addr string) (*muxConn, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	mc := &muxConn{
		c:         c,
		addr:      addr,
		pending:   make(map[uint64]chan *protocol.Response),
		reconnect: make(chan struct{}, 1),
	}
	go mc.readLoop()
	return mc, nil
}

// pick returns the next connection in round-robin order.
func (t *TCPClient) pick() *muxConn {
	i := t.rr.Add(1) - 1
	return t.conns[int(i)%len(t.conns)]
}

// Do sends a request and waits for its response.
func (t *TCPClient) Do(ctx context.Context, op string, payload any) (*protocol.Response, error) {
	mc := t.pick()
	return mc.do(ctx, op, payload)
}

func (mc *muxConn) do(ctx context.Context, op string, payload any) (*protocol.Response, error) {
	if mc.closed.Load() {
		return nil, errors.New("connection closed")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	id := mc.nextID.Add(1)
	req := &protocol.Request{
		ReqID: id,
		Op:    op,
		Data:  b,
	}
	ch := make(chan *protocol.Response, 1)
	mc.pendingMu.Lock()
	mc.pending[id] = ch
	mc.pendingMu.Unlock()

	defer func() {
		mc.pendingMu.Lock()
		delete(mc.pending, id)
		mc.pendingMu.Unlock()
	}()

	if err := protocol.EncodeRequest(mc.c, &mc.writeMu, req); err != nil {
		mc.markBroken()
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readLoop consumes response frames and dispatches them to waiters.
func (mc *muxConn) readLoop() {
	scanner := protocol.NewFrameReader(mc.c)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		resp, err := protocol.DecodeResponse(line)
		if err != nil {
			log.Printf("tcpclient decode: %v", err)
			continue
		}
		mc.pendingMu.Lock()
		ch, ok := mc.pending[resp.ReqID]
		mc.pendingMu.Unlock()
		if ok {
			select {
			case ch <- resp:
			default:
			}
		}
	}
	// Connection ended
	if err := scanner.Err(); err != nil {
		log.Printf("tcpclient read err: %v", err)
	}
	mc.markBroken()
}

// markBroken closes the connection and fails all pending waiters.
func (mc *muxConn) markBroken() {
	if !mc.closed.CompareAndSwap(false, true) {
		return
	}
	_ = mc.c.Close()
	mc.pendingMu.Lock()
	for id, ch := range mc.pending {
		close(ch)
		delete(mc.pending, id)
	}
	mc.pendingMu.Unlock()
	// (Reconnection logic could be added here; kept simple for v1.)
}

// Close closes all pooled connections.
func (t *TCPClient) Close() error {
	for _, mc := range t.conns {
		if mc != nil {
			mc.markBroken()
		}
	}
	return nil
}
