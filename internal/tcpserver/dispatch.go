// TODO: Op-code → handler dispatch table.
//
// Maps protocol.Op* constants to handler functions:
//   OpSignup         → auth.go:handleSignup
//   OpLogin          → auth.go:handleLogin
//   OpLogout         → auth.go:handleLogout
//   OpGetMe          → auth.go:handleGetMe
//   OpUpdateNickname → profile.go:handleUpdateNickname
//   OpUpdateAvatar   → profile.go:handleUpdateAvatar
//
// Public API:
//   func dispatch(ctx, req protocol.Request) protocol.Response
//
// On unknown op: return status="err" with error="unknown op".
package tcpserver
