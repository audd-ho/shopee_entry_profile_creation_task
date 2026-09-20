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
