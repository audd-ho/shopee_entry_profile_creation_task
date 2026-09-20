// TODO: Persistent multiplexed TCP client pool (HTTP server → TCP server).
//
// Design (see §4.7 of system_design.md):
//   - Maintain N (8-16) persistent TCP connections to the TCP server
//   - Multiplex concurrent requests over each connection via req_id
//   - One reader goroutine per connection; matches responses to waiters by req_id
//   - Write side: serialize with per-connection mutex OR channel of outgoing frames
//
// Data structures:
//
//	type Pool struct { conns []*muxConn; roundRobinIdx atomic.Uint64 }
//	type muxConn struct {
//	    conn      net.Conn
//	    writeMu   sync.Mutex
//	    nextID    atomic.Uint64
//	    pending   map[uint64]chan protocol.Response
//	    pendingMu sync.Mutex
//	    closed    chan struct{}
//	}
//
// Public API:
//
//	func NewPool(addr string, size int) (*Pool, error)
//	func (p *Pool) Do(ctx context.Context, req protocol.Request) (protocol.Response, error)
//	func (p *Pool) Close() error
//
// Timeouts:
//   - Per-request: 2s (context deadline)
//   - Reconnect on connection death (drop pending, return error to callers)
package httpserver

import (
	"context"
	"encoding/json"
	"entry_profile_creation/internal/protocol"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

type LockDataTCPConnection struct {
	TCPAddress      string
	cfw             *protocol.ConnectionFrameWriter
	mu              sync.Mutex // within the same connection
	reqIdChannelMap map[uint64]chan protocol.Response
	reqId           atomic.Uint64
}

func NewLockDataTCPConnection(TCPConnectionAddress string) *LockDataTCPConnection {
	log.Printf("Establishing TCP Connection to %v", TCPConnectionAddress)
	connection, err := net.Dial("tcp", TCPConnectionAddress)
	if err != nil {
		log.Printf("Establishing TCP Connection to %v: failed", TCPConnectionAddress)
	}
	log.Printf("Establishing TCP Connection to %v: success", TCPConnectionAddress)

	return &LockDataTCPConnection{
		TCPAddress: TCPConnectionAddress,
		cfw:        protocol.NewConnectionFrameWriter(connection),
	}
}

type TCPConnectionPool struct {
	ldconns       []*LockDataTCPConnection
	roundRobinIdx atomic.Uint64 // for the different connections
}

func NewTCPConnectionPool(TCPConnectionAddress string, size int) *TCPConnectionPool {
	return &TCPConnectionPool{
		//.. make size number of them
		// round robin id initialise at 0 for % size to load balance
	}
}

func (TCPcp *TCPConnectionPool) getConn() *LockDataTCPConnection {
	return TCPcp.ldconns[TCPcp.roundRobinIdx.Add(1)]
}

//intermediaates

func (TCPcp *TCPConnectionPool) Call(ctx context.Context, op string, data, resp any) error {
	useConn := TCPcp.getConn()
	reqId := useConn.reqId.Add(1)
	dataJson, err := json.Marshal(data)
	if err != nil {
		log.Printf("reqId %v data json marshalling error, %v", reqId, err)
		return err
	}
	respChan := make(chan protocol.Response, 1) // buffered with 1 to ensure reader gorotine can non-blockingly drop the patyload and move on if ctx times out or cancel while request in flight

	// order matters, else if response came but no map chan value to go to
	useConn.mu.Lock()
	useConn.reqIdChannelMap[reqId] = respChan
	useConn.mu.Unlock()

	defer func() {
		useConn.mu.Lock()
		delete(useConn.reqIdChannelMap, reqId)
		useConn.mu.Unlock()
	}()

	if err = useConn.cfw.WriteRequest(&protocol.Request{ReqId: reqId, Op: op, Data: dataJson}); err != nil {
		log.Printf("reqId %v write request failed: %w", reqId, err)
		return fmt.Errorf("write request failed: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case respData := <-respChan:
		if respData.Error != "" {
			return errors.New(respData.Error)
		}

		// 5. Unmarshal raw json bytes into the output parameter 'resp'
		if err := json.Unmarshal(respData.Data, resp); err != nil {
			return fmt.Errorf("failed to unmarshal response payload: %w", err)
		}

		return nil
	}
}

// channel to pass to the pool and pass back from pool to goroutine it belongs to, to return to user
// not sure how optimal this is???
