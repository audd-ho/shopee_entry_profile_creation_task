package config

import (
	"strings"
	"testing"
	"time"
)

func TestCheckRequestTimeouts(t *testing.T) {
	base := func() *Config {
		return &Config{
			HTTP: HTTPConfig{WriteTimeout: 15 * time.Second, BackendTimeout: 5 * time.Second},
			TCP:  TCPConfig{RequestTimeout: 4 * time.Second},
		}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"defaults nest", func(*Config) {}, ""},
		{"no write timeout", func(c *Config) { c.HTTP.WriteTimeout = 0 }, ""},
		{"write timeout not above backend", func(c *Config) { c.HTTP.WriteTimeout = 5 * time.Second }, "HTTP_WRITE_TIMEOUT_SECONDS"},
		{"backend not above TCP request", func(c *Config) { c.TCP.RequestTimeout = 5 * time.Second }, "HTTP_BACKEND_TIMEOUT_MS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.CheckRequestTimeouts()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want one mentioning %s", err, tc.wantErr)
			}
		})
	}
}
