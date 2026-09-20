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

const requestIdKey ctxKey = iota

func requestIdMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqId := "req-" + "" // random int or something

		ctx := context.WithValue(r.Context(), requestIdKey, reqId)

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

func (lrw loggingResponseWriter) WriteHeader(code int) {
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

// check if cookie present or not // login just dont use/add this middleware since login no cookie ?
func authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("session_id") == "" {

			http.Error(w, http.StatusText(http.StatusUnauthorized)+": Cookie Missing", http.StatusUnauthorized)
		}

		h.ServeHTTP(w, r)
	})
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
