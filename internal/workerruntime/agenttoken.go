package workerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// IssuedToken is the hub's POST /auth/token answer.
type IssuedToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	TTL       int       `json:"ttl_seconds"`
}

// TokenMinter asks the hub's issuer for agent tokens, authenticating as this
// outpost's paired device (the device.token file, re-read on every request so a
// renewed device credential is picked up).
type TokenMinter struct {
	Hub             string
	DeviceTokenFile string
	Client          *http.Client
	// TTL asks for a specific token lifetime (0 = the issuer's default, 15m).
	TTL time.Duration
}

// Mint requests an agent token bound to session sid.
func (m *TokenMinter) Mint(ctx context.Context, aud string, scopes []string, sid string) (IssuedToken, error) {
	device, err := identity.ReadTokenFile(m.DeviceTokenFile)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("device credential: %w", err)
	}
	payload := map[string]any{"audience": aud, "scopes": scopes, "session_id": sid}
	if m.TTL > 0 {
		payload["ttl_seconds"] = int(m.TTL.Seconds())
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.Hub, "/")+"/auth/token", bytes.NewReader(body))
	if err != nil {
		return IssuedToken{}, err
	}
	req.Header.Set("Authorization", "Bearer "+device)
	req.Header.Set("Content-Type", "application/json")
	c := m.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req) //nolint:gosec // G704: the hub URL is the enrolled outpost configuration
	if err != nil {
		return IssuedToken{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode != http.StatusOK {
		// The hub's error body never contains a credential.
		return IssuedToken{}, fmt.Errorf("hub refused agent token for %s: status %d: %s", aud, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out IssuedToken
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return IssuedToken{}, fmt.Errorf("hub returned an unreadable agent token")
	}
	return out, nil
}

// TokenFilePath is where the outpost keeps a session's token for audience aud.
func TokenFilePath(authDir, aud string) string { return filepath.Join(authDir, aud+".token") }

// TokenFileEnv names the variable that carries the token file PATH (never the
// token) to the agent process.
func TokenFileEnv(aud string) string {
	return "HOLLIS_AUTH_TOKEN_FILE_" + strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, aud))
}

// refreshFraction: the token is rewritten once it is older than 4/9 of its
// TTL; tokenfile.Keep checks every half of that, so the rewrite lands between
// 4/9 and 2/3 of the TTL and never later.
func refreshAfter(ttl time.Duration) time.Duration { return ttl * 4 / 9 }

// startAgentTokens writes each token file once (a failure fails the launch)
// and keeps them fresh until the returned stop is called. It returns the
// environment entries naming the files.
func (e *Executor) startAgentTokens(sid, authDir string, specs []workerwire.AgentTokenSpec) (env []string, stop func(), err error) {
	if len(specs) == 0 || e.Tokens == nil {
		return nil, func() {}, nil
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop = func() {
		cancel()
		_ = os.RemoveAll(authDir)
	}
	for _, spec := range specs {
		path := TokenFilePath(authDir, spec.Aud)
		first, err := e.Tokens.Mint(ctx, spec.Aud, spec.Scopes, sid)
		if err != nil {
			stop()
			return nil, nil, err
		}
		if err := tokenfile.Write(path, first.Token); err != nil {
			stop()
			return nil, nil, err
		}
		ttl := time.Duration(first.TTL) * time.Second
		gen := func() (string, error) {
			t, err := e.Tokens.Mint(ctx, spec.Aud, spec.Scopes, sid)
			return t.Token, err
		}
		go tokenfile.Keep(ctx, path, refreshAfter(ttl), gen, func(_ string, err error) {
			if err != nil {
				log.Printf("session %s: agent token refresh for %s failed: %v", sid, spec.Aud, err)
			}
		})
		env = append(env, TokenFileEnv(spec.Aud)+"="+path)
	}
	return env, stop, nil
}
