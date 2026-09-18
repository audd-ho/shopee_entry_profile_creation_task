package tcpserver

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"sync"

	"entry_profile_creation/internal/protocol"
)

// handleConn processes frames from a single TCP client (the HTTP server).
// Multiple in-flight requests share this connection via req_id multiplexing.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	var writeMu sync.Mutex
	scanner := protocol.NewFrameReader(conn)

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...) // copy — buffer is reused
		req, err := protocol.DecodeRequest(line)
		if err != nil {
			log.Printf("decode request: %v", err)
			continue
		}
		go s.processRequest(ctx, conn, &writeMu, req)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("scanner error: %v", err)
	}
}

// processRequest dispatches a single request and writes the response.
func (s *Server) processRequest(ctx context.Context, conn net.Conn, mu *sync.Mutex, req *protocol.Request) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in op=%s: %v", req.Op, r)
			resp := &protocol.Response{
				ReqID:  req.ReqID,
				Status: protocol.StatusErr,
				Error:  "internal error",
			}
			_ = protocol.EncodeResponse(conn, mu, resp)
		}
	}()

	resp := s.dispatch(ctx, req)
	resp.ReqID = req.ReqID
	if err := protocol.EncodeResponse(conn, mu, resp); err != nil {
		log.Printf("write response: %v", err)
	}
}

// errResponse builds an error response with a message.
func errResponse(reqID uint64, msg string) *protocol.Response {
	return &protocol.Response{
		ReqID:  reqID,
		Status: protocol.StatusErr,
		Error:  msg,
	}
}

// okResponse builds a success response with a data payload.
func okResponse(reqID uint64, data any) *protocol.Response {
	b, err := json.Marshal(data)
	if err != nil {
		return errResponse(reqID, "internal marshal error")
	}
	return &protocol.Response{
		ReqID:  reqID,
		Status: protocol.StatusOK,
		Data:   b,
	}
}
