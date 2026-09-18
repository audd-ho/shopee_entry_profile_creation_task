package tcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"entry_profile_creation/internal/crypto"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// handleUpdateNickname changes the authenticated user's nickname.
func (s *Server) handleUpdateNickname(ctx context.Context, req *protocol.Request) *protocol.Response {
	var data protocol.UpdateNicknameData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return errResponse(req.ReqID, "invalid request")
	}
	sess, err := s.cache.GetSession(ctx, data.Token)
	if err != nil {
		return errResponse(req.ReqID, "unauthenticated")
	}
	nickname, err := validate.Nickname(data.Nickname)
	if err != nil {
		return errResponse(req.ReqID, err.Error())
	}
	if err := db.UpdateNickname(ctx, s.pool, sess.UserID, nickname); err != nil {
		return errResponse(req.ReqID, "database error")
	}
	// Delete-on-write invalidation (see §4.1) — only user:<user_id> needs clearing;
	// creds:<username> stays valid because password didn't change.
	_ = s.cache.InvalidateUser(ctx, sess.UserID)
	return okResponse(req.ReqID, protocol.UpdateNicknameResult{Nickname: nickname})
}

// handleUpdateAvatar accepts image bytes, writes them to disk, and updates DB metadata.
func (s *Server) handleUpdateAvatar(ctx context.Context, req *protocol.Request) *protocol.Response {
	var data protocol.UpdateAvatarData
	if err := json.Unmarshal(req.Data, &data); err != nil {
		return errResponse(req.ReqID, "invalid request")
	}
	sess, err := s.cache.GetSession(ctx, data.Token)
	if err != nil {
		return errResponse(req.ReqID, "unauthenticated")
	}

	imageBytes, err := base64.StdEncoding.DecodeString(data.ImageB64)
	if err != nil {
		return errResponse(req.ReqID, "invalid image encoding")
	}
	mime, err := validate.Image(imageBytes)
	if err != nil {
		return errResponse(req.ReqID, err.Error())
	}
	ext := validate.ExtensionForMIME(mime)

	// Generate random suffix
	suffix, err := crypto.AvatarSuffix()
	if err != nil {
		return errResponse(req.ReqID, "internal error")
	}

	// Atomic version increment + suffix update in DB
	newVersion, err := db.UpdateAvatar(ctx, s.pool, sess.UserID, suffix)
	if err != nil {
		return errResponse(req.ReqID, "database error")
	}

	// Write file (temp then rename for atomicity)
	bucket := sess.UserID >> 14
	dir := filepath.Join(s.cfg.Uploads.Root, fmt.Sprintf("%d", bucket), fmt.Sprintf("%d", sess.UserID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errResponse(req.ReqID, "storage error")
	}
	filename := fmt.Sprintf("%d_v%d_%s.%s", sess.UserID, newVersion, suffix, ext)
	finalPath := filepath.Join(dir, filename)
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, imageBytes, 0o644); err != nil {
		return errResponse(req.ReqID, "storage error")
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return errResponse(req.ReqID, "storage error")
	}

	// Invalidate user display cache (creds:<username> untouched — password unchanged)
	_ = s.cache.InvalidateUser(ctx, sess.UserID)

	url := fmt.Sprintf("/uploads/%d/%d/%d_v%d_%s.%s",
		bucket, sess.UserID, sess.UserID, newVersion, suffix, ext)

	return okResponse(req.ReqID, protocol.UpdateAvatarResult{
		AvatarURL: url,
		Version:   newVersion,
	})
}
