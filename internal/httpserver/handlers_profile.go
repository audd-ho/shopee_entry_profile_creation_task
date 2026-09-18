package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// handleUpdateNickname POST /api/profile/nickname
func (s *Server) handleUpdateNickname(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := ReadSessionCookie(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var body struct {
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.tcpCli.Do(ctx, protocol.OpUpdateNickname, protocol.UpdateNicknameData{
		Token:    token,
		Nickname: body.Nickname,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "backend error")
		return
	}
	if resp.Status != protocol.StatusOK {
		writeErr(w, http.StatusBadRequest, resp.Error)
		return
	}
	writeJSON(w, http.StatusOK, resp.Data)
}

// handleUpdateAvatar POST /api/profile/avatar
func (s *Server) handleUpdateAvatar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := ReadSessionCookie(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	// Cap body size early
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Uploads.MaxBytes+64*1024)
	if err := r.ParseMultipartForm(s.cfg.Uploads.MaxBytes + 64*1024); err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "upload too large or malformed")
		return
	}
	file, _, err := r.FormFile("avatar")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing 'avatar' file field")
		return
	}
	defer file.Close()

	imageBytes, err := io.ReadAll(io.LimitReader(file, s.cfg.Uploads.MaxBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read error")
		return
	}
	if int64(len(imageBytes)) > s.cfg.Uploads.MaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "image too large")
		return
	}
	if _, err := validate.Image(imageBytes); err != nil {
		writeErr(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	resp, err := s.tcpCli.Do(ctx, protocol.OpUpdateAvatar, protocol.UpdateAvatarData{
		Token:    token,
		ImageB64: base64.StdEncoding.EncodeToString(imageBytes),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "backend error")
		return
	}
	if resp.Status != protocol.StatusOK {
		writeErr(w, http.StatusBadRequest, resp.Error)
		return
	}
	writeJSON(w, http.StatusOK, resp.Data)
}
