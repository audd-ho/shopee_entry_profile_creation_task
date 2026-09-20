// TODO: Op-code → handler dispatch table.
//
// Maps protocol.Op* constants to handler functions:
//
//	OpSignup         → auth.go:handleSignup
//	OpLogin          → auth.go:handleLogin
//	OpLogout         → auth.go:handleLogout
//	OpGetMe          → auth.go:handleGetMe
//	OpUpdateNickname → profile.go:handleUpdateNickname
//	OpUpdateAvatar   → profile.go:handleUpdateAvatar
//
// Public API:
//
//	func dispatch(ctx, req protocol.Request) protocol.Response
//
// On unknown op: data = status="err" with error="unknown op".
package tcpserver

import (
	"context"
	"encoding/json"
	"entry_profile_creation/internal/protocol"
)

func (s *Server) dispatch(ctx context.Context, req *protocol.Request) *protocol.Response {
	var data any
	switch req.Op {
	case protocol.OpPing:
		var pingReq protocol.PingReq
		json.Unmarshal(req.Data, &pingReq)
		data = s.handlePing(ctx, &pingReq)
	default:
		return errResponse(req.ReqId, "unknown op: "+req.Op)
	}
	return okResponse(req.ReqId, data)
	/*
		var data any
		switch req.Op {
		case protocol.OpSignup:
			data = s.handleSignup(ctx, req)
		case protocol.OpLogin:
			data = s.handleLogin(ctx, req)
		case protocol.OpLogout:
			data = s.handleLogout(ctx, req)
		case protocol.OpGetUser:
			data = s.handleGetUser(ctx, req)
		case protocol.OpUpdateNickname:
			data = s.handleUpdateNickname(ctx, req)
		case protocol.OpUpdateAvatar:
			data = s.handleUpdateAvatar(ctx, req)
		default:
			return errResponse(req.ReqId, "unknown op: "+req.Op)
		}
		return okResponse(req.ReqId, data)
	*/
}
