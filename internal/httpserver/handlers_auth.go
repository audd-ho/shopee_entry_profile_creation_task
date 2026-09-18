// TODO: Auth-related HTTP handlers.
//
// Handlers:
//   - Signup(w, r)
//       Parse {username, password, nickname} from JSON body
//       Validate via internal/validate
//       Forward to TCP server op=OpSignup
//       Return 201 on success, 4xx on validation/duplicate
//
//   - Login(w, r)
//       Parse {username, password} from JSON body
//       Forward to TCP server op=OpLogin
//       On success: set session cookie, return 200 with user info
//       On failure: return 401 (uniform message, don't reveal user existence)
//
//   - Logout(w, r)
//       Read session token from cookie
//       Forward to TCP server op=OpLogout (invalidates session in Redis)
//       Clear cookie, return 204
//
//   - Me(w, r)
//       Auth already checked by middleware; user_id + username in context
//       Forward to TCP server op=OpGetMe with user_id
//       Return 200 with {username, nickname, avatar_url}
//       avatar_url is derived: /uploads/<id>>>14/<id>/<id>_v<n>_<suffix>.jpg,
//       or /default.png if avatar_version=0
package httpserver
