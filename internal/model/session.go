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

import "time"

// Session is the server-side state behind a session cookie.
//
// The token is opaque: it carries no user data and is not signed, it is simply
// a random lookup key into Redis. That means there is nothing for a client to
// forge or tamper with, and logout is a real DEL that revokes the session
// immediately — which a self-contained signed token cannot offer.
type Session struct {
	// Token is the Redis key rather than part of the stored value, hence
	// `json:"-"`. The cache layer populates it on read.
	Token string `json:"-"`

	UserID uint64 `json:"user_id"`

	CreatedAt time.Time `json:"created_at"`

	// LastSeen is refreshed on activity to drive the sliding-window TTL.
	LastSeen time.Time `json:"last_seen"`

	// IP is best-effort, for anomaly detection only. Never trusted for authz.
	IP string `json:"ip,omitempty"`
}
