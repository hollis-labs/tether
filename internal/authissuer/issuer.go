package authissuer

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	auth "github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/tether/internal/identity"
)

// TTL bounds (design: default 15 min, max 60 min).
const (
	DefaultTTL = 15 * time.Minute
	MaxTTL     = 60 * time.Minute
	// DenyHorizon is how long a revoked sid stays on the deny-list: the longest
	// a token for it could still be valid, plus verifier leeway.
	DenyHorizon = MaxTTL + 5*time.Minute
	// TetherAudience is the aud of tokens for Tether's own API.
	TetherAudience = "tether"
)

// Errors with a stable meaning for the HTTP layer.
var (
	ErrForbidden = errors.New("authissuer: exchange refused")
	ErrInvalid   = errors.New("authissuer: invalid exchange request")
)

func forbidden(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, a...))
}
func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var (
	audRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	scopeRe = regexp.MustCompile(`^([a-z][a-z0-9-]{0,31})\.([a-z][a-z0-9_-]{0,31})$`)
	sidRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Issuer mints and revokes tokens.
type Issuer struct {
	Name  string // iss claim
	Keys  *KeyStore
	State *State
	Now   func() time.Time
}

// New builds an Issuer.
func New(name string, keys *KeyStore, state *State) *Issuer {
	if name == "" {
		name = "tether"
	}
	return &Issuer{Name: name, Keys: keys, State: state, Now: time.Now}
}

// Claims is the JWT payload.
type Claims struct {
	Iss   string `json:"iss"`
	Sub   string `json:"sub"`
	Aud   string `json:"aud"`
	Scope string `json:"scope"`
	Exp   int64  `json:"exp"`
	Iat   int64  `json:"iat"`
	JTI   string `json:"jti"`
	Kind  string `json:"kind"`
	SID   string `json:"sid,omitempty"`
	Par   string `json:"par,omitempty"`
}

// Request is a token exchange request.
type Request struct {
	Audience  string
	Scopes    []string
	TTL       time.Duration // 0 = DefaultTTL
	SessionID string        // non-empty mints an agent token bound to the session
}

// Token is an issued token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	TTL       int       `json:"ttl_seconds"`
	Audience  string    `json:"aud"`
	Scopes    []string  `json:"scopes"`
	Kind      string    `json:"kind"`
	KID       string    `json:"kid"`
	// JTI is the token id (the jti claim): what the deny-list and the audit row
	// name. It is not a credential.
	JTI string `json:"jti"`
}

// SessionPrincipalID is the principal id of an agent session.
func SessionPrincipalID(sid string) string { return "msg://session/local/" + sid }

// Exchange validates req against parent and mints a token.
//
// Parents: the local operator, a service principal, or a paired device
// (tth_ credentials). An issued token, in particular an agent token, never
// mints. Requested scopes must be a subset of the parent's, with one
// exception that is itself a narrowing: a device asking for an agent token for
// a session may receive up to what the hub granted that session at launch.
func (i *Issuer) Exchange(parent identity.Principal, req Request) (Token, error) {
	if !parent.Verified {
		return Token{}, forbidden("unverified caller")
	}
	if parent.TokenID != "" {
		return Token{}, forbidden("issued tokens cannot mint tokens")
	}
	switch parent.Kind {
	case "operator", "service", "device":
	case "agent":
		return Token{}, forbidden("agent tokens cannot mint tokens")
	default:
		return Token{}, forbidden("principal kind %q cannot mint tokens", parent.Kind)
	}
	if !audRe.MatchString(req.Audience) {
		return Token{}, invalid("audience must be one app id")
	}
	scopes, err := normalizeScopes(req.Audience, req.Scopes)
	if err != nil {
		return Token{}, err
	}
	ttl := req.TTL
	switch {
	case ttl == 0:
		ttl = DefaultTTL
	case ttl < 0:
		return Token{}, invalid("ttl must be positive")
	case ttl > MaxTTL:
		return Token{}, invalid("ttl exceeds the %s maximum", MaxTTL)
	}
	now := i.Now().UTC()
	exp := now.Add(ttl)
	if parent.ExpiresAt != nil && parent.ExpiresAt.Before(exp) {
		exp = parent.ExpiresAt.UTC()
	}
	c := Claims{Iss: i.Name, Aud: req.Audience, Scope: strings.Join(scopes, " "), Iat: now.Unix(), JTI: uuid.NewString(), Par: parent.ID}
	if req.SessionID == "" {
		if !identity.ScopesCover(parent.Scopes, scopes) {
			return Token{}, forbidden("requested scopes exceed the parent's")
		}
		c.Sub, c.Kind = parent.ID, parent.Kind
	} else {
		if !sidRe.MatchString(req.SessionID) {
			return Token{}, invalid("invalid session id")
		}
		if i.State.Denied(DenySID, req.SessionID) {
			return Token{}, forbidden("session has ended")
		}
		switch parent.Kind {
		case "device", "service":
			g, ok := i.State.GrantFor(req.SessionID)
			if !ok || g.Launcher != parent.ID {
				return Token{}, forbidden("no launch grant for this session on this %s", parent.Kind)
			}
			if !identity.ScopesCover(g.Scopes[req.Audience], scopes) {
				return Token{}, forbidden("requested scopes exceed the session's launch grant")
			}
			if g.Expires.Before(exp) {
				exp = g.Expires.UTC()
			}
		default: // operator (local socket only): bounded by its own scopes
			if !identity.ScopesCover(parent.Scopes, scopes) {
				return Token{}, forbidden("requested scopes exceed the parent's")
			}
		}
		c.Sub, c.Kind, c.SID = SessionPrincipalID(req.SessionID), auth.KindAgent, req.SessionID
	}
	if !exp.After(now.Add(time.Second)) {
		return Token{}, forbidden("parent credential is about to expire")
	}
	c.Exp = exp.Unix()
	tok, kid, err := i.sign(c)
	if err != nil {
		return Token{}, err
	}
	return Token{Token: tok, ExpiresAt: exp, TTL: int(exp.Sub(now).Seconds()), Audience: c.Aud, Scopes: scopes, Kind: c.Kind, KID: kid, JTI: c.JTI}, nil
}

func (i *Issuer) sign(c Claims) (token, kid string, err error) {
	kid, priv := i.Keys.Active()
	hdr, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	pl, err := json.Marshal(c)
	if err != nil {
		return "", "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hdr) + "." + enc.EncodeToString(pl)
	return signing + "." + enc.EncodeToString(ed25519.Sign(priv, []byte(signing))), kid, nil
}

// normalizeScopes validates scopes for an audience: each is <aud>.<verb>, no
// wildcards. For Tether's own audience the bare device scopes and the legacy
// MCP child scopes are accepted and mapped into the tether.* vocabulary.
func normalizeScopes(aud string, in []string) ([]string, error) {
	if len(in) == 0 || len(in) > 16 {
		return nil, invalid("request between 1 and 16 scopes")
	}
	if aud == TetherAudience {
		out, err := identity.CanonicalScopes(in)
		if err != nil {
			return nil, invalid("unknown tether scope")
		}
		return out, nil
	}
	seen := map[string]bool{}
	for _, s := range in {
		m := scopeRe.FindStringSubmatch(s)
		if m == nil || m[1] != aud {
			return nil, invalid("scope %q must be %s.<verb>", s, aud)
		}
		seen[s] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	slices.Sort(out)
	return out, nil
}

// RegisterSessionGrant records what the outpost may mint for a session. scopes
// maps audience to scopes; Tether's audience is canonicalized.
func (i *Issuer) RegisterSessionGrant(sid, device string, scopes map[string][]string, expires time.Time) error {
	norm := map[string][]string{}
	for aud, sc := range scopes {
		if !audRe.MatchString(aud) {
			return invalid("audience")
		}
		n, err := normalizeScopes(aud, sc)
		if err != nil {
			return err
		}
		norm[aud] = n
	}
	return i.State.Grant(sid, SessionGrant{Launcher: device, Scopes: norm, Expires: expires.UTC()})
}

// RevokeSession ends a session's tokens: its sid goes on the deny-list (for
// as long as a token for it could live) and its grant is dropped, so refresh
// requests are refused too.
func (i *Issuer) RevokeSession(sid string) error {
	if sid == "" {
		return nil
	}
	err := i.State.Deny(DenySID, sid, i.Now().Add(DenyHorizon))
	return errors.Join(err, i.State.DropGrant(sid))
}

// Rotate rotates the signing key.
func (i *Issuer) Rotate() error { return i.Keys.Rotate() }

// DefaultRotateEvery is the signing-key lifetime before automatic rotation.
const DefaultRotateEvery = 30 * 24 * time.Hour

// Maintain rotates the active key once it is older than rotateEvery and
// deletes keys past their overlap window, hourly, until ctx is done.
func (i *Issuer) Maintain(ctx context.Context, rotateEvery time.Duration) {
	step := func() {
		if rotateEvery > 0 && i.Keys.ActiveAge() >= rotateEvery {
			if err := i.Keys.Rotate(); err != nil {
				log.Printf("authissuer: key rotation failed: %v", err)
			}
		}
		if _, err := i.Keys.Prune(); err != nil {
			log.Printf("authissuer: key prune failed: %v", err)
		}
	}
	step()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			step()
		}
	}
}
