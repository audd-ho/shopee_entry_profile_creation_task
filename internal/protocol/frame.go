// TODO: HTTP↔TCP wire protocol framing.
//
// Framing: newline-delimited JSON (v1).
//
//	Request:  {"req_id": <int>, "op": "<string>", "data": {...}}\n
//	Response: {"req_id": <int>, "status": "ok"|"err", "error": "...", "data": {...}}\n
//
// Responsibilities:
//   - Define Request and Response struct types with json tags
//   - Provide Encode(w io.Writer, msg) and Decode(r io.Reader) helpers
//   - Use bufio.Scanner or bufio.Reader with '\n' delimiter for read side
//   - Ensure single-line JSON (no pretty-print) so newline is unambiguous
//
// Future (stretch): swap for length-prefixed binary framing.
// Shared by both cmd/http-server (client side) and cmd/tcp-server (server side).
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type Request struct {
	ReqId uint64          `json:"req_id"`
	Op    string          `json:"op"` // int also works? maybe even better for parsing?
	Data  json.RawMessage `json:"data"`
}

type Response struct {
	ReqId  uint64          `json:"req_id"`
	Status string          `json:"status"`
	Error  string          `json:"error,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// MaxFrameBytes bounds a single frame to avoid pathological reads.
// Set high enough to accommodate a 2 MB avatar upload plus JSON overhead.
// Not used for now since json.Decoder has no size ceiling, this max bytes is more for bufio scanner with sc.Buffer(nil, maxframebytes)
//const MaxFrameBytes = 4 * 1024 * 1024

// free-list idea to prevent head of the line blocking due to one single connection (affect other connections)
// possible but over engineered so KIV for now

type ConnectionFrameWriter struct {
	mu  sync.Mutex
	bw  *bufio.Writer // buffer for io
	enc *json.Encoder
}

func NewConnectionFrameWriter(w io.Writer) *ConnectionFrameWriter {
	bw := bufio.NewWriter(w)
	return &ConnectionFrameWriter{bw: bw, enc: json.NewEncoder(bw)}
}

// http server side
func (cfw *ConnectionFrameWriter) WriteRequest(req *Request) error {
	cfw.mu.Lock()
	defer cfw.mu.Unlock()

	if err := cfw.enc.Encode(req); err != nil { // writes the JSON + '\n'
		return fmt.Errorf("encode request: %w", err)
	}
	return cfw.bw.Flush()
}

// tcp server side
func (cfw *ConnectionFrameWriter) WriteResponse(resp *Response) error {
	cfw.mu.Lock()
	defer cfw.mu.Unlock()

	if err := cfw.enc.Encode(resp); err != nil { // writes the JSON + '\n'
		return fmt.Errorf("encode request: %w", err)
	}
	return cfw.bw.Flush()
}

type ConnectionFrameReader struct {
	dec *json.Decoder // reader no need mutex, each reader handle one reader goroutine
}

func NewConnectionFrameReader(r io.Reader) *ConnectionFrameReader {
	return &ConnectionFrameReader{dec: json.NewDecoder(bufio.NewReader(r))}
}

// tcp server side
func (cfr *ConnectionFrameReader) ReadRequest(req *Request) error {
	// take from connection, reader and decode into req
	if err := cfr.dec.Decode(req); err != nil {
		return err // return io.EOF unwrapped, callers can check for it
	}
	return nil
}

// http server side
func (cfr *ConnectionFrameReader) ReadResponse(resp *Response) error {
	// take from connection, reader and decode into resp
	return cfr.dec.Decode(resp)
}
