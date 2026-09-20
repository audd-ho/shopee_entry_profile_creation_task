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
	"time"
)

type LockDataTCPConnection struct {
	TCPAddress      string
	cfw             *protocol.ConnectionFrameWriter
	mu              sync.Mutex // within the same connection
	reqIdChannelMap map[uint64]chan *protocol.Response
	reqId           atomic.Uint64
	done            chan struct{} // signal a dead connection (Closed when readConnectionLoop exits)
}

func NewLockDataTCPConnection(TCPConnectionAddress string) (*LockDataTCPConnection, error) {
	log.Printf("Establishing TCP Connection to %v", TCPConnectionAddress)
	connection, err := net.Dial("tcp", TCPConnectionAddress)
	if err != nil {
		log.Printf("Dial to %v: failed, %w", TCPConnectionAddress, err)
		return nil, fmt.Errorf("Dial to %v: failed, %w", TCPConnectionAddress, err)
	}
	log.Printf("Dial to %v: success", TCPConnectionAddress)

	ldTCPc := &LockDataTCPConnection{
		TCPAddress:      TCPConnectionAddress,
		cfw:             protocol.NewConnectionFrameWriter(connection),
		reqIdChannelMap: make(map[uint64]chan *protocol.Response),
		done:            make(chan struct{}), // initialised channel for the exit signal
	}

	go ldTCPc.readConnectionLoop(connection)

	return ldTCPc, nil
}

func (ldTCPc *LockDataTCPConnection) readConnectionLoop(connection net.Conn) {
	cfr := protocol.NewConnectionFrameReader(connection)

	defer func() {
		connection.Close()
		ldTCPc.cleanupPendingRequests(errors.New("connection closed"))
		close(ldTCPc.done) // signal maintainer loop to reconnect!
	}()

	for {
		resp, err := cfr.ReadResponse()
		if err != nil {
			log.Printf("Connection to %s lost: %v", ldTCPc.TCPAddress, err)
			// clean up remaining request that rely or waiting on this connection
			ldTCPc.cleanupPendingRequests(err)
			return
		}

		ldTCPc.mu.Lock()
		ch, ok := ldTCPc.reqIdChannelMap[resp.ReqId]
		ldTCPc.mu.Unlock()

		if ok {
			ch <- resp // unblocks the waiting Call() method in its goroutine
		}
	}
}

// Unblocks any goroutines waiting on this connection if it drops unexpectedly
func (ldTCPc *LockDataTCPConnection) cleanupPendingRequests(err error) {
	// 1. Lock briefly just to copy channels and drain the map
	ldTCPc.mu.Lock()
	pending := make(map[uint64]chan *protocol.Response, len(ldTCPc.reqIdChannelMap))
	for reqId, ch := range ldTCPc.reqIdChannelMap {
		pending[reqId] = ch
		delete(ldTCPc.reqIdChannelMap, reqId) // Safe to delete here
	}
	ldTCPc.mu.Unlock() // Release lock BEFORE channel sends!

	// 2. Unblock callers without holding the mutex
	errResp := &protocol.Response{Error: fmt.Sprintf("connection closed: %v", err)}
	for _, ch := range pending {
		select {
		case ch <- errResp:
		default: // Prevents blocking if channel buffer is full or caller abandoned
		}
	}
}

type TCPConnectionPool struct {
	ldconns       []*LockDataTCPConnection
	roundRobinIdx atomic.Uint64 // for the different connections
}

func NewTCPConnectionPool(TCPConnectionAddress string, size int) *TCPConnectionPool {
	pool := &TCPConnectionPool{
		ldconns: make([]*LockDataTCPConnection, size),
	}

	return pool
}

func (TCPcp *TCPConnectionPool) maintainSlotConnection(slotIdx int, addr string) {
	go func() {
		for {
			connection, err := NewLockDataTCPConnection(addr)
			if err != nil {
				log.Printf("[Pool] Slot %d connection to %s failed: %v. Retrying in 1s...", slotIdx, addr, err)
				time.Sleep(1 * time.Second)
				continue
			}

			// Atomically place the active connection into the slot
			// Note: Replace with atomic.Pointer or a Mutex if accessed across goroutines mid-write
			TCPcp.ldconns[slotIdx] = connection

			// Wait until this specific connection dies before attempting next loop iteration
			<-connection.done
			log.Printf("[Pool] Slot %d connection died. Reconnecting...", slotIdx)
		}
	}()
}

// in case no connections at all
func (TCPcp *TCPConnectionPool) getConn() (*LockDataTCPConnection, error) {
	totalConnections := uint64(len(TCPcp.ldconns))
	// check all 8, possible for all 8 to die maybe?, then prevent infinite loop
	// and if one dead, can at least move on to the next
	for i := 0; i < int(totalConnections); i++ {
		idx := TCPcp.roundRobinIdx.Add(1)
		curConn := TCPcp.ldconns[idx%totalConnections]

		if curConn != nil {
			select {
			case <-curConn.done:
				// connection is dead/reconnecting, check next slot
				continue
			default:
				return curConn, nil
			}
		}
	}

	return nil, errors.New("no active TCP connections available in pool")
}

//intermediaates

func (TCPcp *TCPConnectionPool) Call(ctx context.Context, op string, data, resp any) error {
	useConn, err := TCPcp.getConn()
	if err != nil {
		log.Printf("No connections for use right now (all DEAD), %v", err)
		return err
	}
	reqId := useConn.reqId.Add(1)
	dataJson, err := json.Marshal(data)
	if err != nil {
		log.Printf("reqId %v data json marshalling error, %v", reqId, err)
		return err
	}
	respChan := make(chan *protocol.Response, 1) // buffered with 1 to ensure reader gorotine can non-blockingly drop the patyload and move on if ctx times out or cancel while request in flight

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
