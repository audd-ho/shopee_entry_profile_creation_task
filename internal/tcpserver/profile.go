// TODO: Profile update business logic on the TCP server.
//
// Handlers:
//
//   - handleUpdateNickname(data)
//     Validate nickname (defense in depth): UTF-8, 1-32 runes, strip control chars
//     UPDATE users SET nickname = ? WHERE id = ?
//     Invalidate cache:
//     DEL profile:<user_id>
//     DEL user:<username>       (username in session, forwarded via data)
//     Return ok
//
//   - handleUpdateAvatar(data)
//     Validate: size ≤ 2 MB, MIME allowlist (jpeg/png/webp) via http.DetectContentType
//     Generate 8-hex random suffix (crypto/random.go)
//     Atomically increment version + set suffix:
//     UPDATE users SET
//     avatar_version = LAST_INSERT_ID(avatar_version + 1),
//     avatar_suffix = ?
//     WHERE id = ?
//     SELECT LAST_INSERT_ID() to get new_version
//     Compute path: /uploads/<user_id>>>14/<user_id>/<user_id>_v<new_version>_<suffix>.jpg
//     Ensure directory exists (os.MkdirAll, idempotent)
//     Write to <path>.tmp, then os.Rename to <path> (atomic on POSIX)
//     Invalidate cache:
//     DEL profile:<user_id>
//     DEL user:<username>
//     Old files become orphans; async sweeper cleans them up periodically.
//     Return {new_version, new_avatar_url}
package tcpserver

// ---------------------------------------------------------------------------
// Phase 5 implementation.
//
// Both handlers take a session token rather than a user id. The HTTP server
// never learns who the caller is — it forwards the cookie verbatim — so the
// identity used for the write is resolved here, from Redis, on every request.
// Accepting a user_id over the wire would let anything that can reach this port
// edit any account.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"entry_profile_creation/internal/crypto"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/model"
	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// handleUpdateNickname sets a new display name.
func (s *Server) handleUpdateNickname(ctx context.Context, d *protocol.UpdateNicknameData) (*protocol.UpdateNicknameResult, error) {
	session, err := s.resolveSession(ctx, d.Token)
	if err != nil {
		return nil, err
	}

	// Re-validated here for the same reason signup is: the HTTP server is just
	// another client of this port, and the cleaned form it returns (control
	// characters stripped, trimmed) is what gets stored, not the raw input.
	nickname, err := validate.Nickname(d.Nickname)
	if err != nil {
		return nil, protocol.ErrCodeInvalidInput
	}

	if err := db.UpdateNickname(ctx, s.db, session.UserID, nickname); err != nil {
		log.Printf("update_nickname: user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	// After the write, and non-fatal. The row is already correct; a failed DEL
	// only means a reader may see the old nickname until the TTL expires, which
	// is not worth failing a succeeded update over.
	if err := s.cache.InvalidateOnNicknameChange(ctx, session.UserID, session.Username); err != nil {
		log.Printf("update_nickname: invalidate user %d: %v", session.UserID, err)
	}

	return &protocol.UpdateNicknameResult{Nickname: nickname}, nil
}

// handleUpdateAvatar stores a new profile picture and bumps its version.
//
// Ordering is deliberate: bump the version in MySQL first, then write the file
// at the path that version implies. Doing it the other way round would mean a
// crash between the two steps leaves the row pointing at a file that was never
// written — a broken image for that user until they upload again. This order
// fails the other way: an orphaned file nobody references, which costs disk and
// is swept up later.
func (s *Server) handleUpdateAvatar(ctx context.Context, d *protocol.UpdateAvatarData) (*protocol.UpdateAvatarResult, error) {
	session, err := s.resolveSession(ctx, d.Token)
	if err != nil {
		return nil, err
	}

	raw, err := base64.StdEncoding.DecodeString(d.ImageB64)
	if err != nil {
		return nil, protocol.ErrCodeInvalidInput
	}
	// Size and MIME are checked again here, not just at the HTTP edge, because
	// this is where the bytes get a filename and land on disk.
	if int64(len(raw)) > s.cfg.Uploads.MaxBytes {
		return nil, protocol.ErrCodeInvalidInput
	}
	if _, err := validate.Image(raw); err != nil {
		return nil, protocol.ErrCodeInvalidInput
	}

	// The suffix makes the path unguessable, so that knowing a user id is not
	// enough to enumerate avatars, and so a re-upload at the same version (which
	// cannot happen, but) cannot collide with the previous file.
	suffix, err := crypto.AvatarSuffix()
	if err != nil {
		log.Printf("update_avatar: generate suffix: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	version, err := db.UpdateAvatar(ctx, s.db, session.UserID, suffix)
	switch {
	case errors.Is(err, db.ErrUserNotFound):
		return nil, protocol.ErrCodeUnauthorized
	case err != nil:
		log.Printf("update_avatar: user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	if err := s.writeAvatarFile(session.UserID, version, suffix, raw); err != nil {
		log.Printf("update_avatar: write file for user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	if err := s.cache.InvalidateOnAvatarChange(ctx, session.UserID, session.Username); err != nil {
		log.Printf("update_avatar: invalidate user %d: %v", session.UserID, err)
	}

	return &protocol.UpdateAvatarResult{
		AvatarURL: model.AvatarURL(session.UserID, version, suffix),
		Version:   version,
	}, nil
}

// writeAvatarFile puts the bytes at the path derived from (id, version, suffix).
//
// The write is to a temporary name followed by os.Rename, which is atomic on
// POSIX within a filesystem. Writing in place would let the static file server
// serve a half-written image to anyone who requests it during the copy. The
// temp file is created in the destination directory, not /tmp, because rename
// across filesystems is not atomic and would silently degrade to a copy.
func (s *Server) writeAvatarFile(userID uint64, version uint32, suffix string, data []byte) error {
	rel := filepath.FromSlash(model.AvatarRelPath(userID, version, suffix))
	full := filepath.Join(s.cfg.Uploads.Root, rel)

	dir := filepath.Dir(full)
	// MkdirAll is idempotent, so this costs one stat on the common path where
	// the shard directory already exists.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".avatar-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Removes the temp file on any failure path below; a no-op once the rename
	// has moved it away.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// Close before rename, and check its error: a buffered write can fail at
	// close, and renaming a file whose write failed would publish a truncated
	// image.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	// CreateTemp makes the file 0600, which the static file server can read
	// (same user) but is stricter than the rest of the tree.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("rename to %s: %w", full, err)
	}
	return nil
}
