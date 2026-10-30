package app

import (
	"testing"
	"time"
)

func TestSessionTokenTTL(t *testing.T) {
	SetSessionTokenTTLSetting("")
	cases := []struct {
		name, env, cfg string
		want           time.Duration
	}{
		{"default", "", "", 7 * 24 * time.Hour},
		{"env override", "48h", "", 48 * time.Hour},
		{"config", "", "12h", 12 * time.Hour},
		{"env beats config", "1h", "12h", time.Hour},
		{"never", "never", "", neverSessionTokenTTL},
		{"zero", "0", "", neverSessionTokenTTL},
		{"invalid env falls back", "bogus", "", DefaultSessionTokenTTL},
		{"negative falls back", "-5h", "", DefaultSessionTokenTTL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(SessionTokenTTLEnv, c.env)
			SetSessionTokenTTLSetting(c.cfg)
			defer SetSessionTokenTTLSetting("")
			if got := sessionTokenTTL(); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
