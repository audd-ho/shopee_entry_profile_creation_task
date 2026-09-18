package tcpserver

import (
	"context"

	"entry_profile_creation/internal/protocol"
)

// dispatch routes a request to the appropriate handler based on Op.
func (s *Server) dispatch(ctx context.Context, req *protocol.Request) *protocol.Response {
	switch req.Op {
	case protocol.OpSignup:
		return s.handleSignup(ctx, req)
	case protocol.OpLogin:
		return s.handleLogin(ctx, req)
	case protocol.OpLogout:
		return s.handleLogout(ctx, req)
	case protocol.OpGetUser:
		return s.handleGetUser(ctx, req)
	case protocol.OpUpdateNickname:
		return s.handleUpdateNickname(ctx, req)
	case protocol.OpUpdateAvatar:
		return s.handleUpdateAvatar(ctx, req)
	default:
		return errResponse(req.ReqID, "unknown op: "+req.Op)
	}
}
