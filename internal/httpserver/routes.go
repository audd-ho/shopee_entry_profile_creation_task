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
	mux.HandleFunc("/api/signup", s.handleSignup)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/user", s.handleUser)
	mux.HandleFunc("/api/profile/nickname", s.handleUpdateNickname)
	mux.HandleFunc("/api/profile/avatar", s.handleUpdateAvatar)

	return recoverer(logger(mux))
}

// noSniff adds X-Content-Type-Options: nosniff to responses (see §4.4.1).
func noSniff(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.ServeHTTP(w, r)
	})
}
