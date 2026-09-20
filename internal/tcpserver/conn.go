// TODO: Per-TCP-connection handler on the server side.
//
// Responsibilities:
//   - Read frames from the connection in a loop (protocol.Decode)
//   - For each frame: dispatch to the appropriate handler (see dispatch.go)
//   - Write response frames back over the same connection
//     (protect the write side with a mutex — multiple handler goroutines write concurrently)
//   - Support concurrent request handling per connection (goroutine per request)
//   - Handle connection close, malformed frames, panics
//
// Flow:
//
//	for {
//	    req := protocol.Decode(conn)
//	    go func() {
//	        resp := dispatch(req)
//	        writeMu.Lock(); protocol.Encode(conn, resp); writeMu.Unlock()
//	    }()
//	}
package tcpserver

import (
	"context"
	"encoding/json"
	"entry_profile_creation/internal/protocol"
	"errors"
	"io"
	"log"
	"net"
)

// connection context to pass down to requests
func (s *Server) handleConnection(ctx context.Context, connection net.Conn) {
	defer func() {
		log.Printf("Connection closed: %v (local) to %v (remote)", connection.LocalAddr(), connection.RemoteAddr())
		connection.Close()
	}()

	log.Printf("Connection accepted: %v (local) to %v (remote)", connection.LocalAddr(), connection.RemoteAddr())

	cfr := protocol.NewConnectionFrameReader(connection)
	cfw := protocol.NewConnectionFrameWriter(connection)

	for {
		req, err := cfr.ReadRequest()
		if err != nil {
			// check for clean client disconnection (EOF)
			// else, will keep reading socket since readable now that client disconnected, and the EOF will keep being read NOT into buffer, but an error return value + 0 bytes being read/copied, and loop infinitely
			if errors.Is(err, io.EOF) {
				log.Printf("TCP Server: client disconnected cleanly")
				break // Exit the loop to close the connection			}
			}

			log.Printf("TCP Server: read request error: %v", err)
			// skip this request
			//continue

			// cannot skip request, just break connection since using json decoder with no way of recovering from bad decoding (cant throw away bytes/char from internal buffer, and tcp itself cant know when the next good message is, so need http or some framing or delimiter, which decoder itself dont really use other than json but json has its own problems like this, so hard to recover by clearing internal buffer and so just break)
			break
		}

		go s.processRequest(ctx, cfw, req)
	}

}

func (s *Server) processRequest(ctx context.Context, cfw *protocol.ConnectionFrameWriter, req *protocol.Request) {

	log.Printf("Processing Request: ReqId=%v", req.ReqId)

	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in op=%s: %v", req.Op, r)
			resp := errResponse(req.ReqId, "internal error")
			if err := cfw.WriteResponse(resp); err != nil {
				log.Printf("TCP Server: write response error: %v", err)
			}
		}
	}()

	// pure tcp and frames, no header, cant have trace id unless put it inside the frame but thats KIV for future
	//context.WithValue(ctx, "trace_id") unique global, requst not unique since different connections can duplicate, OR NOT, depending on how req id is implemented, i think no duplicate but still prefer to keep separate from trace id?
	//ctx.WithContext()

	resp := s.dispatch(ctx, req)
	resp.ReqId = req.ReqId // overwrites to ensure correctness
	if err := cfw.WriteResponse(resp); err != nil {
		log.Printf("TCP Server: write response error: %v", err)
	}
}

// errResponse builds an error response with a message.
func errResponse(reqID uint64, msg string) *protocol.Response {
	return &protocol.Response{
		ReqId:  reqID,
		Status: protocol.StatusErr,
		Error:  msg,
	}
}

// okResponse builds a success response with a data payload.
func okResponse(reqID uint64, data any) *protocol.Response {
	b, err := json.Marshal(data)
	if err != nil {
		log.Printf("marshal response data for req %d: %v", reqID, err)
		return errResponse(reqID, "internal marshal error")
	}

	if data == nil { // will emit null in json
		return &protocol.Response{
			ReqId:  reqID,
			Status: protocol.StatusOK,
		}
	}

	return &protocol.Response{
		ReqId:  reqID,
		Status: protocol.StatusOK,
		Data:   b,
	}
}
