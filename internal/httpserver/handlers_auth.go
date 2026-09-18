package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"entry_profile_creation/internal/protocol"
)

// handleSignup POST /api/signup
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body protocol.SignupData
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.tcpCli.Do(ctx, protocol.OpSignup, body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "backend error")
		return
	}
	if resp.Status != protocol.StatusOK {
		writeErr(w, http.StatusBadRequest, resp.Error)
		return
	}
	writeJSON(w, http.StatusCreated, resp.Data)
}

// handleLogin POST /api/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body protocol.LoginData
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.IP = clientIP(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.tcpCli.Do(ctx, protocol.OpLogin, body)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "backend error")
		return
	}
	if resp.Status != protocol.StatusOK {
		writeErr(w, http.StatusUnauthorized, resp.Error)
		return
	}
	var result protocol.LoginResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		writeErr(w, http.StatusInternalServerError, "bad backend response")
		return
	}
	SetSessionCookie(w, result.Token, int(s.cfg.Session.TTL.Seconds()))
	// Minimal login response: just success. Frontend calls /api/user next.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"success":true}`))
}

// handleLogout POST /api/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := ReadSessionCookie(r)
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	_, _ = s.tcpCli.Do(ctx, protocol.OpLogout, protocol.LogoutData{Token: token})
	ClearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleUser GET /api/user
func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := ReadSessionCookie(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	resp, err := s.tcpCli.Do(ctx, protocol.OpGetUser, protocol.GetUserData{Token: token})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "backend error")
		return
	}
	if resp.Status != protocol.StatusOK {
		writeErr(w, http.StatusUnauthorized, resp.Error)
		return
	}
	writeJSON(w, http.StatusOK, resp.Data)
}

// --- shared helpers ---

func writeJSON(w http.ResponseWriter, code int, data json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if len(data) == 0 {
		_, _ = w.Write([]byte("{}"))
		return
	}
	_, _ = w.Write(data)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(b)
}

func clientIP(r *http.Request) string {
	// Prefer X-Forwarded-For if behind a proxy; else RemoteAddr
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}
