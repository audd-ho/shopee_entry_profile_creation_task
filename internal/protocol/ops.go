// TODO: Op-code constants for the HTTP↔TCP protocol.
//
// String constants used in Request.Op to identify which handler on the TCP server
// should process the request. Kept in one place to avoid typos.
//
// Ops:
//   OpSignup         = "signup"
//   OpLogin          = "login"
//   OpLogout         = "logout"
//   OpGetMe          = "get_me"           // fetch current user info (for /api/me)
//   OpUpdateNickname = "update_nickname"
//   OpUpdateAvatar   = "update_avatar"
//
// Response statuses:
//   StatusOK  = "ok"
//   StatusErr = "err"
package protocol
