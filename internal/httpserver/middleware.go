// TODO: HTTP middleware chain.
//
// Middleware to implement (in order):
//  1. Recover      — catch panics, return 500, log
//  2. Logger       — structured log per request (method, path, status, duration)
//  3. RequestID    — attach a request ID for tracing
//  4. Session      — read session cookie, look up session in Redis via TCP server,
//     attach user_id + username to request context if valid
//  5. RequireAuth  — reject with 401 if session middleware didn't attach a user
//  6. (Shelved) CSRF double-submit token check
//
// Cookie handling:
//   - Session cookie: HttpOnly, SameSite=Lax, Secure(prod), Path=/, name="session"
//   - Set on login (Set-Cookie header)
//   - Cleared on logout
package httpserver

import (
	"context"
	"entry_profile_creation/internal/protocol"
	"log"
	"net/http"
	"runtime/debug"
	"time"
)

type Middleware func(http.Handler) http.Handler

func Chain(logicFinalInnermostHandler http.Handler, middlewares ...Middleware) http.Handler {
	// loopbackwards with the final being it
	curHandler := logicFinalInnermostHandler
	for i := len(middlewares) - 1; i >= 0; i-- {
		curHandler = middlewares[i](curHandler)
	}
	return curHandler
}

type ctxKey int

const (
	traceIdKey ctxKey = iota
	sessionTokenKey
)

func requestIdMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceId := "traceId-" + "" // random int or something

		ctx := context.WithValue(r.Context(), traceIdKey, traceId)

		h.ServeHTTP(w, r.WithContext(ctx))

		// nothing after
	})
}

func loggingMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(lrw, r)
		callLatency := time.Since(start)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, lrw.status, callLatency)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter // embedded field here
	status              int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.ResponseWriter.WriteHeader(code)
	lrw.status = code
}

func recoverMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// panic message and stack tree for debugging
				log.Printf("PANIC RECOVERED: %v\n%s", err, debug.Stack())

				// respond to user http request
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// authMiddleware requires a session cookie and puts the token on the context.
//
// It deliberately does NOT validate the token. Resolving sess:<token> means a
// Redis lookup, Redis belongs to the TCP server, and asking the TCP server to
// resolve it here would cost a whole extra round trip on every request — after
// which the real handler would immediately make a second call carrying the same
// token. So this only rejects the cheap case (no cookie at all) and forwards
// the token; the handler's own op resolves it once, on the far side.
//
// The consequence to keep in mind: reaching a handler means the request HAD a
// token, not that the token is valid. Handlers must still treat
// ErrCodeUnauthorized from the backend as a real possibility.
func authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ReadSessionCookie(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, string(protocol.ErrCodeUnauthorized))
			return // without this, an unauthenticated request runs the handler anyway
		}

		ctx := context.WithValue(r.Context(), sessionTokenKey, token)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionToken returns the token authMiddleware attached, or "" if the route
// was not behind it.
func sessionToken(r *http.Request) string {
	token, _ := r.Context().Value(sessionTokenKey).(string)
	return token
}

// every other route/function
func MiddlewareWrappers(finalLogicHandler http.Handler) http.Handler {
	return Chain(
		finalLogicHandler,

		requestIdMiddleware,
		loggingMiddleware,
		recoverMiddleware,
		authMiddleware,
	)
}

// for login
func MiddlewareWrappers_NoAuth(finalLogicHandler http.Handler) http.Handler {
	return Chain(
		finalLogicHandler,

		requestIdMiddleware,
		loggingMiddleware,
		recoverMiddleware,
	)
}
