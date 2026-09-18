// TODO: HTTP middleware chain.
//
// Middleware to implement (in order):
//   1. Recover      — catch panics, return 500, log
//   2. Logger       — structured log per request (method, path, status, duration)
//   3. RequestID    — attach a request ID for tracing
//   4. Session      — read session cookie, look up session in Redis via TCP server,
//                     attach user_id + username to request context if valid
//   5. RequireAuth  — reject with 401 if session middleware didn't attach a user
//   6. (Shelved) CSRF double-submit token check
//
// Cookie handling:
//   - Session cookie: HttpOnly, SameSite=Lax, Secure(prod), Path=/, name="session"
//   - Set on login (Set-Cookie header)
//   - Cleared on logout
package httpserver
