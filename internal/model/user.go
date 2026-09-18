// TODO: User model.
//
// Shared type used by db, cache, tcpserver packages.
//
// type User struct {
//     ID            uint64
//     Username      string
//     PasswordHash  []byte     // bcrypt output, 60 bytes
//     Nickname      string
//     AvatarVersion uint32     // 0 = no avatar uploaded
//     AvatarSuffix  string     // 8 hex chars; empty when no avatar
//     CreatedAt     time.Time
//     UpdatedAt     time.Time
// }
//
// Helper: (u *User) AvatarURL() string
//   Returns "/default.png" if AvatarVersion == 0.
//   Otherwise "/uploads/<id>>>14/<id>/<id>_v<version>_<suffix>.jpg"
package model
