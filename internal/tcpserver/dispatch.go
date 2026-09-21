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
	"errors"
	"log"
)

// dispatch routes one request to its handler and turns the (result, error)
// pair into a protocol.Response.
//
// Handlers return a protocol.ErrorCode for anything the client is allowed to
// know about. Any other error is a bug or an outage: it is logged here and
// reported as "internal", so a MySQL message or a stack detail can never reach
// the browser.
func (s *Server) dispatch(ctx context.Context, req *protocol.Request) *protocol.Response {
	var (
		data any
		err  error
	)

	switch req.Op {
	case protocol.OpPing:
		var d protocol.PingReq
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return errResponse(req.ReqId, string(protocol.ErrCodeInvalidInput))
		}
		data = s.handlePing(ctx, &d)

	case protocol.OpSignup:
		var d protocol.SignupData
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return errResponse(req.ReqId, string(protocol.ErrCodeInvalidInput))
		}
		data, err = s.handleSignup(ctx, &d)

	case protocol.OpLogin:
		var d protocol.LoginData
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return errResponse(req.ReqId, string(protocol.ErrCodeInvalidInput))
		}
		data, err = s.handleLogin(ctx, &d)

	case protocol.OpLogout:
		var d protocol.LogoutData
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return errResponse(req.ReqId, string(protocol.ErrCodeInvalidInput))
		}
		data, err = s.handleLogout(ctx, &d)

	case protocol.OpGetUser:
		var d protocol.GetUserData
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return errResponse(req.ReqId, string(protocol.ErrCodeInvalidInput))
		}
		data, err = s.handleGetUser(ctx, &d)

	case protocol.OpUpdateNickname:
		var d protocol.UpdateNicknameData
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return errResponse(req.ReqId, string(protocol.ErrCodeInvalidInput))
		}
		data, err = s.handleUpdateNickname(ctx, &d)

	case protocol.OpUpdateAvatar:
		var d protocol.UpdateAvatarData
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return errResponse(req.ReqId, string(protocol.ErrCodeInvalidInput))
		}
		data, err = s.handleUpdateAvatar(ctx, &d)

	default:
		return errResponse(req.ReqId, "unknown op: "+req.Op)
	}

	if err != nil {
		var code protocol.ErrorCode
		if errors.As(err, &code) {
			return errResponse(req.ReqId, string(code))
		}
		log.Printf("op=%s req=%d unhandled error: %v", req.Op, req.ReqId, err)
		return errResponse(req.ReqId, string(protocol.ErrCodeInternal))
	}

	return okResponse(req.ReqId, data)
}
