// TODO: Session model.
//
// type Session struct {
//     Token     string    // opaque random 32-byte hex (server-side generated)
//     UserID    uint64
//     Username  string    // stored for cache invalidation without extra DB lookup
//     CreatedAt time.Time
//     LastSeen  time.Time // updated on activity for sliding-window refresh
//     IP        string    // optional, for anomaly detection
// }
//
// Serialized as JSON when stored in Redis under key `sess:<token>`.
package model
