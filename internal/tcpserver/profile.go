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
	"time"

	"entry_profile_creation/internal/crypto"
	"entry_profile_creation/internal/db"
	"entry_profile_creation/internal/model"
	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// invalidateTimeout bounds an async cache invalidation. Independent of the
// request's own context (which is cancelled the moment the handler returns),
// and short: a DEL is a single Redis round trip, so anything near this long
// means Redis is in real trouble, not just slow.
const invalidateTimeout = 2 * time.Second

// invalidateAsync runs a cache invalidation in the background so a client's
// response never waits on a Redis round trip that has no bearing on whether
// their write actually succeeded — that was already decided in MySQL by the
// time this fires (system_design.md §4.1, §4.4.4: "[separate goroutine]").
//
// Tracked on the server's own shutdown WaitGroup, so Run's graceful shutdown
// drains outstanding invalidations rather than abandoning them mid-flight.
//
// This does widen the delete-on-write staleness window slightly: a client
// could in principle re-fetch its own just-changed profile before the
// goroutine's DEL has run, and see the value that was cached under the old
// key a moment before. That is not a new failure mode — delete-on-write
// already accepts staleness up to the TTL by design — it just stretches an
// already-accepted window from "effectively zero" to "a goroutine schedule
// delay", which is immaterial next to a 10-minute/1-hour TTL.
func (s *Server) invalidateAsync(what string, fn func(ctx context.Context) error) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), invalidateTimeout)
		defer cancel()
		if err := fn(ctx); err != nil {
			log.Printf("invalidate %s: %v", what, err)
		}
	}()
}

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

	dataPool := s.db.DataShardFor(session.UserID)
	newRowVersion, err := db.UpdateNickname(ctx, dataPool, session.UserID, nickname)
	if err != nil {
		log.Printf("update_nickname: user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	// After the write, and asynchronous (system_design.md §4.1's
	// "[separate goroutine]"): the row is already correct in MySQL, so the
	// client's response does not need to wait on a Redis round trip that
	// cannot change whether the update succeeded. oldRowVersion is exact here
	// (unlike UpdateAvatar's pre-read): row_version = row_version + 1 via
	// LAST_INSERT_ID means newRowVersion is precisely what THIS statement set,
	// so subtracting 1 is not a guess.
	oldRowVersion := newRowVersion - 1
	s.invalidateAsync("nickname", func(ctx context.Context) error {
		return s.cache.InvalidateOnNicknameChange(ctx, session.UserID, oldRowVersion)
	})

	return &protocol.UpdateNicknameResult{Nickname: nickname}, nil
}

// handleUpdateAvatar stores a new profile picture under a fresh random
// filename suffix.
//
// Ordering is deliberate, and the reverse of what an earlier version of this
// design used: write the file FIRST, then update the row that points at it.
// That earlier version bumped an avatar_version column in MySQL before
// writing the file, because the version came from an atomic DB increment and
// the filename needed it — which forced DB-first, and meant a crash between
// the two steps left the row pointing at a version whose file was never
// written: a broken avatar with no rollback, discovered on the client's next
// unrelated page load rather than on the upload that caused it. A suffix
// alone needs no DB round trip to generate (crypto.AvatarSuffix, below), so
// nothing forces that ordering any more: file-first means a failure at the
// DB step leaves an orphaned file behind (harmless — nothing points at it)
// and the row untouched and still valid, which is what "the upload failed"
// should actually mean to a client that already has a working avatar.
//
// This ordering has its own failure mode, not a crash-only one: a file that
// exists on disk before its row exists in MySQL looks exactly like an orphan
// to anything that decides what to keep by asking the database. A periodic
// sweeper built to compare against current DB state — the natural way to
// write one — could delete this file in the ordinary window between the
// rename below landing and the UpdateAvatar call below committing, with no
// crash involved at all. See model.AvatarSweepGracePeriod for the fix any
// such sweeper must apply (skip anything younger than a fixed age,
// unconditionally) — there is no sweeper in this codebase yet, so this is a
// requirement on whoever writes the next one, not a bug in this handler.
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

	// Generated with no DB round trip — crypto/rand, not a MySQL sequence —
	// which is exactly what makes writing the file before touching the row
	// possible. The suffix also makes the path unguessable, so knowing a user
	// id is not enough to enumerate avatars.
	suffix, err := crypto.AvatarSuffix()
	if err != nil {
		log.Printf("update_avatar: generate suffix: %v", err)
		return nil, protocol.ErrCodeInternal
	}

	if err := s.writeAvatarFile(session.UserID, suffix, raw); err != nil {
		log.Printf("update_avatar: write file for user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	// If this fails, the file just written is an orphan and the row is
	// untouched — the client is correctly told the upload failed, and unlike
	// the old DB-first order, nothing in MySQL now disagrees with that.
	dataPool := s.db.DataShardFor(session.UserID)
	newRowVersion, err := db.UpdateAvatar(ctx, dataPool, session.UserID, suffix)
	switch {
	case errors.Is(err, db.ErrUserNotFound):
		return nil, protocol.ErrCodeUnauthorized
	case err != nil:
		log.Printf("update_avatar: user %d: %v", session.UserID, err)
		return nil, protocol.ErrCodeInternal
	}

	// Asynchronous, same reasoning as handleUpdateNickname. oldRowVersion is
	// exact here too, now that row_version is the only atomic counter this
	// statement touches — see db.UpdateAvatar's comment.
	oldRowVersion := newRowVersion - 1
	s.invalidateAsync("avatar", func(ctx context.Context) error {
		return s.cache.InvalidateOnAvatarChange(ctx, session.UserID, oldRowVersion)
	})

	return &protocol.UpdateAvatarResult{
		AvatarURL: model.AvatarURL(session.UserID, suffix),
	}, nil
}

// writeAvatarFile puts the bytes at the path derived from (id, suffix).
//
// The write is to a temporary name followed by os.Rename, which is atomic on
// POSIX within a filesystem. Writing in place would let the static file server
// serve a half-written image to anyone who requests it during the copy. The
// temp file is created in the destination directory, not /tmp, because rename
// across filesystems is not atomic and would silently degrade to a copy.
func (s *Server) writeAvatarFile(userID uint64, suffix string, data []byte) error {
	rel := filepath.FromSlash(model.AvatarRelPath(userID, suffix))
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
