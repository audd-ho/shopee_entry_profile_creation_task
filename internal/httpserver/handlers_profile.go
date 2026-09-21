// TODO: Profile-edit HTTP handlers.
//
// Handlers:
//
//   - Nickname(w, r)
//     Auth required (checked by middleware)
//     Parse {nickname} from JSON body
//     Validate: UTF-8, 1-32 runes, strip control chars
//     Forward to TCP server op=OpUpdateNickname with user_id + nickname
//     Return 200 on success
//
//   - Avatar(w, r)
//     Auth required
//     Parse multipart form with "avatar" file field
//     Enforce max size (2 MB) via http.MaxBytesReader BEFORE full read
//     Sniff MIME with http.DetectContentType on first 512 bytes
//     Allowlist: image/jpeg, image/png, image/webp
//     (Stretch: image.Decode for dimension check + malformed detection)
//     Forward to TCP server op=OpUpdateAvatar with user_id + raw image bytes
//     TCP server writes to filesystem and updates DB
//     Return 200 with new avatar URL
package httpserver

// ---------------------------------------------------------------------------
// Phase 5 implementation.
//
// Both handlers sit behind authMiddleware, so the session cookie is present by
// the time they run. They still do not decide who the user is: the token is
// forwarded and the TCP server resolves it. This process holds no Redis
// connection and cannot tell a valid token from a forged one.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"

	"entry_profile_creation/internal/protocol"
	"entry_profile_creation/internal/validate"
)

// multipartMemory is how much of an upload ParseMultipartForm keeps in RAM
// before spilling to a temp file. Sized just above the 2 MB avatar limit so the
// normal case never touches disk, while MaxBytesReader still caps the total.
const multipartMemory = 3 << 20

// handleUpdateNickname changes the display name. POST /api/profile/nickname
func (s *Server) handleUpdateNickname(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Nickname string `json:"nickname"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}

	// Validated here to give a specific message, and again on the TCP server
	// because that is where the trust boundary actually is. The cleaned string
	// is sent on, so the backend validates an already-normalised value.
	nickname, err := validate.Nickname(body.Nickname)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()

	var result protocol.UpdateNicknameResult
	if err := s.tcpClientPool.Call(ctx, protocol.OpUpdateNickname, &protocol.UpdateNicknameData{
		Token:    sessionToken(r),
		Nickname: nickname,
	}, &result); err != nil {
		backendError(w, "update_nickname", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// handleUpdateAvatar replaces the profile picture. POST /api/profile/avatar
func (s *Server) handleUpdateAvatar(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader wraps the body before anything reads it, so an oversized
	// upload is cut off mid-stream and the server never buffers more than the
	// limit. Checking len() after reading would mean a client could make this
	// process allocate a gigabyte first and be told "too large" second.
	//
	// The slack above MaxBytes covers multipart framing: the boundary lines and
	// part headers are part of the request body but not of the image.
	maxBody := s.cfg.Uploads.MaxBytes + (64 << 10)
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, validate.ErrImageTooLarge.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}
	// Frees the temp files ParseMultipartForm may have created once the handler
	// returns; without this they survive until GC notices, which under load is
	// a slow disk leak.
	defer func() {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			log.Printf("update_avatar: clean up multipart temp files: %v", err)
		}
	}()

	file, _, err := r.FormFile("avatar")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing avatar file")
		return
	}
	defer file.Close()

	// Bounded by MaxBytesReader above, so this cannot grow unbounded. The
	// filename and the part's Content-Type are both ignored: only the bytes
	// decide what this is.
	raw, err := readAllLimited(file, s.cfg.Uploads.MaxBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, validate.ErrImageTooLarge.Error())
		return
	}

	if _, err := validate.Image(raw); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()

	// Base64 costs a third more bytes on the wire, which is the price of the
	// newline-delimited JSON framing in protocol/frame.go: raw image bytes
	// would contain newlines and break the frame boundary. A length-prefixed
	// binary frame is the listed stretch goal, and this is the call site that
	// would benefit from it.
	var result protocol.UpdateAvatarResult
	if err := s.tcpClientPool.Call(ctx, protocol.OpUpdateAvatar, &protocol.UpdateAvatarData{
		Token:    sessionToken(r),
		ImageB64: base64.StdEncoding.EncodeToString(raw),
	}, &result); err != nil {
		backendError(w, "update_avatar", err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// readAllLimited reads at most max bytes and reports an error if the source had
// more. It reads max+1 on purpose: that extra byte is the only way to tell "the
// file is exactly at the limit" from "the file is over it", without trusting a
// Content-Length the client controls.
func readAllLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, validate.ErrImageTooLarge
	}
	return data, nil
}
