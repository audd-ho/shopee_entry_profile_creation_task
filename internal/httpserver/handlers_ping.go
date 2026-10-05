package httpserver

import (
	"context"
	"encoding/json"
	"entry_profile_creation/internal/protocol"
	"net/http"
)

// curl "localhost:8080/api/ping?id=123"
func (s *Server) handleGETPing(w http.ResponseWriter, r *http.Request) {

	ctx, cancel := context.WithTimeout(r.Context(), s.backendTimeout())
	defer cancel()

	var pingResp protocol.PingResp
	err := s.tcpClientPool.Call(ctx, protocol.OpPing, &protocol.PingReq{Hi: "hello GET world (" + r.URL.Query().Get("id") + ")"}, &pingResp)
	if err != nil {
		http.Error(w, "Internal Server Error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(pingResp.Bye))

}

// curl -X POST localhost:8080/api/ping -d '{"post_test":"456"}'
func (s *Server) handlePOSTPing(w http.ResponseWriter, r *http.Request) {

	ctx, cancel := context.WithTimeout(r.Context(), s.backendTimeout())
	defer cancel()

	type pingBody struct {
		PostTest string `json:"post_test"`
	}
	var reqBody pingBody
	err := json.NewDecoder(r.Body).Decode(&reqBody)
	if err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	var pingResp protocol.PingResp
	err = s.tcpClientPool.Call(ctx, protocol.OpPing, &protocol.PingReq{Hi: "hello POST world (" + reqBody.PostTest + ")"}, &pingResp)
	if err != nil {
		http.Error(w, "Internal Server Error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(pingResp.Bye))

}
