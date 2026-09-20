package tcpserver

import (
	"context"
	"entry_profile_creation/internal/protocol"
)

func (s *Server) handlePing(ctx context.Context, pingReq *protocol.PingReq) *protocol.PingResp {

	pingResp := &protocol.PingResp{Bye: "PINGED: " + pingReq.Hi + " GOT IT"}
	return pingResp
}
