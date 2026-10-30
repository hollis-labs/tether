package app

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultSessionTokenTTL is the session tth_ token lifetime when nothing is set.
const DefaultSessionTokenTTL = 7 * 24 * time.Hour

// SessionTokenTTLEnv overrides the configured identity.session_token_ttl.
const SessionTokenTTLEnv = "TETHER_SESSION_TOKEN_TTL"

// neverSessionTokenTTL stands in for "no expiry": ten years.
const neverSessionTokenTTL = 10 * 365 * 24 * time.Hour

var configuredSessionTokenTTL atomic.Value // string

// SetSessionTokenTTLSetting records identity.session_token_ttl from config.
func SetSessionTokenTTLSetting(v string) { configuredSessionTokenTTL.Store(v) }

func sessionTokenTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv(SessionTokenTTLEnv))
	src := SessionTokenTTLEnv
	if raw == "" {
		if v, ok := configuredSessionTokenTTL.Load().(string); ok {
			raw = strings.TrimSpace(v)
		}
		src = "identity.session_token_ttl"
	}
	return parseSessionTokenTTL(raw, src)
}

func parseSessionTokenTTL(raw, src string) time.Duration {
	if raw == "" {
		return DefaultSessionTokenTTL
	}
	if raw == "0" || strings.EqualFold(raw, "never") {
		return neverSessionTokenTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("invalid session token ttl; using default", "source", src, "default", DefaultSessionTokenTTL.String())
		return DefaultSessionTokenTTL
	}
	return d
}
