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

import (
	"net/http"
	"os"
	"time"
)

// SessionCookieName is the cookie the browser carries. Nothing else in the
// service should hardcode this string.
const SessionCookieName = "session"

// secureCookies adds the Secure attribute, which tells the browser never to
// send the cookie over plain HTTP. It must be off for local development on
// http://localhost — with it on the browser silently drops the cookie and login
// appears to succeed while every subsequent request is anonymous.
//
// Set COOKIE_SECURE=1 anywhere the service is behind TLS.
var secureCookies = os.Getenv("COOKIE_SECURE") == "1"

// SetSessionCookie issues the session cookie after a successful login.
//
// The attributes are the actual security boundary, so each is deliberate:
//
//   - HttpOnly keeps the token out of document.cookie, so a single XSS bug
//     cannot exfiltrate every visitor's session.
//   - SameSite=Lax stops another origin's form POST from riding along with the
//     cookie, which is most of CSRF handled for free. Lax rather than Strict so
//     that following a link into the site still arrives logged in.
//   - Path=/ because every API route needs it.
//   - MaxAge mirrors the Redis TTL, so the browser stops sending a cookie at
//     roughly the moment the server would stop honouring it. The server side
//     remains authoritative; this only avoids pointless round trips.
func SetSessionCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// ReadSessionCookie returns the session token, or "" when the cookie is absent.
//
// The token is opaque to the HTTP server: it performs no parsing, no signature
// check and no lookup. Validation belongs to the TCP server, which owns the
// sess:<token> record in Redis.
func ReadSessionCookie(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil { // http.ErrNoCookie is the only error this returns
		return ""
	}
	return c.Value
}

// ClearSessionCookie deletes the cookie on logout.
//
// MaxAge is negative, which is how a cookie is deleted: there is no "unset"
// header, only an instruction to overwrite it with an already-expired one. The
// Path, Secure and SameSite attributes must match the ones used when setting
// it, or the browser treats this as a different cookie and leaves the original
// in place.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}
