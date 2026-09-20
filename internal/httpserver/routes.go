// TODO: HTTP route table.
//
// Endpoints (see §6 of system_design.md):
//
//	GET  /                        landing (login/signup UI, served from static)
//	POST /api/signup              handlers_auth.go:Signup
//	POST /api/login               handlers_auth.go:Login
//	POST /api/logout              handlers_auth.go:Logout        (auth required)
//	GET  /api/me                  handlers_auth.go:Me            (auth required)
//	POST /api/profile/nickname    handlers_profile.go:Nickname   (auth required)
//	POST /api/profile/avatar      handlers_profile.go:Avatar     (auth required)
//	GET  /uploads/*               static file serving of avatars
//	GET  /default.png             default avatar for users with avatar_version=0
//
// Static routes:
//
//	GET  /login.html, /signup.html, /profile.html, /style.css, /app.js
//	  served from the frontend directory
package httpserver

import "net/http"

func (s *Server) buildRouter() http.Handler {
	mux := http.NewServeMux()

	// Static web assets
	fs := http.FileServer(http.Dir(s.cfg.HTTP.WebRoot))
	mux.Handle("/", fs)

	// Serve uploaded avatars
	uploadsFS := http.FileServer(http.Dir(s.cfg.Uploads.Root))
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", noSniff(uploadsFS)))

	// API endpoints
	mux.HandleFunc("GET /api/ping", s.handleGETPing)
	mux.HandleFunc("POST /api/ping", s.handlePOSTPing)

	/*
		mux.HandleFunc("/api/signup", s.handleSignup)
		mux.HandleFunc("/api/login", s.handleLogin)

		mux.HandleFunc("/api/logout", authMiddleware(s.handleLogout))
		mux.HandleFunc("/api/user", authMiddleware(s.handleUser))
		mux.HandleFunc("/api/profile/nickname", authMiddleware(s.handleUpdateNickname))
		mux.HandleFunc("/api/profile/avatar", authMiddleware(s.handleUpdateAvatar))
	*/

	return MiddlewareWrappers_NoAuth(mux)
}

// noSniff adds X-Content-Type-Options: nosniff to responses (see §4.4.1).
func noSniff(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}
