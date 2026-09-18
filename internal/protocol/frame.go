// Package protocol defines the wire format for the HTTP↔TCP internal protocol.
//
// Framing: newline-delimited JSON.
//   Request:  {"req_id": <int>, "op": "<string>", "data": {...}}\n
//   Response: {"req_id": <int>, "status": "ok"|"err", "error": "...", "data": {...}}\n
package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Request is a message sent from the HTTP server to the TCP server.
type Request struct {
	ReqID uint64          `json:"req_id"`
	Op    string          `json:"op"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Response is a message sent from the TCP server to the HTTP server.
type Response struct {
	ReqID  uint64          `json:"req_id"`
	Status string          `json:"status"`
	Error  string          `json:"error,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// MaxFrameBytes bounds a single frame to avoid pathological reads.
// Set high enough to accommodate a 2 MB avatar upload plus JSON overhead.
const MaxFrameBytes = 4 * 1024 * 1024

// EncodeRequest writes a request as a single newline-terminated JSON line.
// The write is guarded by mu (each shared connection needs a write mutex).
func EncodeRequest(w io.Writer, mu *sync.Mutex, req *Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	b = append(b, '\n')
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	_, err = w.Write(b)
	return err
}

// EncodeResponse writes a response as a single newline-terminated JSON line.
func EncodeResponse(w io.Writer, mu *sync.Mutex, resp *Response) error {
	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	b = append(b, '\n')
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	_, err = w.Write(b)
	return err
}

// NewFrameReader returns a bufio.Scanner configured for our framing.
// Scanner returns one JSON message per call to Scan().
func NewFrameReader(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	s.Buffer(buf, MaxFrameBytes)
	return s
}

// DecodeRequest parses a raw frame into a Request.
func DecodeRequest(line []byte) (*Request, error) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, fmt.Errorf("unmarshal request: %w", err)
	}
	return &req, nil
}

// DecodeResponse parses a raw frame into a Response.
func DecodeResponse(line []byte) (*Response, error) {
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return &resp, nil
}
