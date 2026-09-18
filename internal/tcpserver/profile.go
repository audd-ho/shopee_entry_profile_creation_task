// TODO: Profile update business logic on the TCP server.
//
// Handlers:
//   - handleUpdateNickname(data)
//       Validate nickname (defense in depth): UTF-8, 1-32 runes, strip control chars
//       UPDATE users SET nickname = ? WHERE id = ?
//       Invalidate cache:
//         DEL profile:<user_id>
//         DEL user:<username>       (username in session, forwarded via data)
//       Return ok
//
//   - handleUpdateAvatar(data)
//       Validate: size ≤ 2 MB, MIME allowlist (jpeg/png/webp) via http.DetectContentType
//       Generate 8-hex random suffix (crypto/random.go)
//       Atomically increment version + set suffix:
//         UPDATE users SET
//           avatar_version = LAST_INSERT_ID(avatar_version + 1),
//           avatar_suffix = ?
//         WHERE id = ?
//         SELECT LAST_INSERT_ID() to get new_version
//       Compute path: /uploads/<user_id>>>14/<user_id>/<user_id>_v<new_version>_<suffix>.jpg
//       Ensure directory exists (os.MkdirAll, idempotent)
//       Write to <path>.tmp, then os.Rename to <path> (atomic on POSIX)
//       Invalidate cache:
//         DEL profile:<user_id>
//         DEL user:<username>
//       Old files become orphans; async sweeper cleans them up periodically.
//       Return {new_version, new_avatar_url}
package tcpserver
