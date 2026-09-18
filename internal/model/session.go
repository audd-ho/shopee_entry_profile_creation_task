package model

import "time"

// Session represents an authenticated user session stored in Redis under `sess:<token>`.
type Session struct {
	Token     string    `json:"-"` // token is the Redis key, not stored in the value
	UserID    uint64    `json:"user_id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	IP        string    `json:"ip,omitempty"`
}
