// TODO: Session cookie helpers.
//
// Helpers:
//   - SetSessionCookie(w, token, ttl) — Set-Cookie with HttpOnly, SameSite=Lax, Path=/
//   - ReadSessionCookie(r) — extract token from "session" cookie, return "" if absent
//   - ClearSessionCookie(w) — Set-Cookie with MaxAge=-1 to delete
//
// The session token is opaque to the HTTP server — it's just a string.
// The TCP server owns the sess:<token> Redis record.
// The HTTP middleware forwards the token to TCP server for validation.
package httpserver
