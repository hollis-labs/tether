package authissuer

import (
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/identity"
)

// Service tokens (CW-20261019-0044, security direction v1): a third token class
// for an app or workload. It is an Ed25519 JWT with kind=service and a very
// distant exp (go-auth rejects exp==0, so "no expiry" is implemented as ten
// years). It is revoked through the deny-list, not by expiring, which is why the
// 60 minute issuer cap does not apply to it and applies to nothing else here:
// Exchange is untouched and still caps every other token.
const (
	// KindService is the kind claim of a service token.
	KindService = "service"
	// ServiceTokenLifetime is the exp of a service token ("no expiry").
	ServiceTokenLifetime = 10 * 365 * 24 * time.Hour
	// DefaultServiceEnv is the env segment of the service principal when none is given.
	DefaultServiceEnv = "dev"
	maxServiceRecords = 256
)

// serviceTetherScopes is the dev allowlist of Tether scopes a service token may
// hold: publish events, read, and the message APIs. Nothing that administers
// Tether, launches sessions or touches the catalog.
var serviceTetherScopes = []string{"tether.events", "tether.read", "tether.messages"}

var (
	envRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/-]{0,63}$`)
)

// ServiceRequest asks for a service token.
type ServiceRequest struct {
	// App names the workload. The principal is msg://service/<app>/<env>.
	App string
	// Env defaults to "dev".
	Env string
	// Audience is the app that will verify the token: "tether" for Tether's own
	// API (publishing events), or another app id. Required.
	Audience string
	// Scopes are <audience>.<verb>. Explicit verbs only, unless AllowWildcard.
	Scopes []string
	// Label is a short human label for listings.
	Label string
	// AllowWildcard permits <audience>.* (dev only; logged as a warning).
	AllowWildcard bool
}

// ServiceRecord is the issuer's metadata for one service token. It never holds
// the token.
type ServiceRecord struct {
	JTI       string     `json:"jti"`
	Sub       string     `json:"sub"`
	App       string     `json:"app"`
	Env       string     `json:"env"`
	Audience  string     `json:"aud"`
	Scopes    []string   `json:"scopes"`
	Label     string     `json:"label,omitempty"`
	IssuedBy  string     `json:"issued_by"`
	IssuedAt  time.Time  `json:"issued_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ServicePrincipalID is the principal id (the sub claim) of a service token.
func ServicePrincipalID(app, env string) string { return "msg://service/" + app + "/" + env }

// IssueService mints a service token for req. Only the verified local operator
// may issue one (the caller checks that the operator credential came over the
// local socket). The record is persisted before the token is returned: if it
// cannot be recorded, nothing is minted.
func (i *Issuer) IssueService(parent identity.Principal, req ServiceRequest) (Token, ServiceRecord, error) {
	if !parent.Verified || parent.Kind != "operator" || parent.TokenID != "" {
		return Token{}, ServiceRecord{}, forbidden("only the operator issues service tokens")
	}
	if !audRe.MatchString(req.App) || identity.ReservedIssuerSubject(ServicePrincipalID(req.App, DefaultServiceEnv)) {
		return Token{}, ServiceRecord{}, invalid("app must be one app id")
	}
	env := req.Env
	if env == "" {
		env = DefaultServiceEnv
	}
	if !envRe.MatchString(env) {
		return Token{}, ServiceRecord{}, invalid("env must be one lowercase id")
	}
	if !audRe.MatchString(req.Audience) || identity.ReservedEnrollmentAudience(req.Audience) {
		return Token{}, ServiceRecord{}, invalid("audience must be one app id")
	}
	if req.Label != "" && !labelRe.MatchString(req.Label) {
		return Token{}, ServiceRecord{}, invalid("label must be at most 64 plain characters")
	}
	scopes, err := serviceScopes(req)
	if err != nil {
		return Token{}, ServiceRecord{}, err
	}
	sub := ServicePrincipalID(req.App, env)
	if identity.ReservedIssuerSubject(sub) {
		return Token{}, ServiceRecord{}, forbidden("issuer protocol subjects cannot be service principals")
	}
	if n := i.State.serviceCount(); n >= maxServiceRecords {
		return Token{}, ServiceRecord{}, forbidden("too many live service tokens (%d); revoke unused ones", n)
	}
	now := i.Now().UTC()
	exp := now.Add(ServiceTokenLifetime)
	c := Claims{Iss: i.Name, Sub: sub, Aud: req.Audience, Scope: strings.Join(scopes, " "), Exp: exp.Unix(), Iat: now.Unix(),
		JTI: uuid.NewString(), Kind: KindService, Par: parent.ID}
	tok, kid, err := i.sign(c)
	if err != nil {
		return Token{}, ServiceRecord{}, err
	}
	rec := ServiceRecord{JTI: c.JTI, Sub: sub, App: req.App, Env: env, Audience: req.Audience, Scopes: scopes, Label: req.Label,
		IssuedBy: parent.ID, IssuedAt: now, ExpiresAt: exp}
	if err := i.State.addService(rec); err != nil {
		return Token{}, ServiceRecord{}, fmt.Errorf("authissuer: cannot record the service token: %w", err)
	}
	if slices.ContainsFunc(scopes, func(s string) bool { return strings.HasSuffix(s, ".*") }) {
		log.Printf("authissuer: WARNING service token jti=%s sub=%s holds a wildcard scope %q (dev only)", rec.JTI, sub, scopes)
	}
	return Token{Token: tok, ExpiresAt: exp, TTL: int(exp.Sub(now).Seconds()), Audience: c.Aud, Scopes: scopes, Kind: KindService, KID: kid, JTI: c.JTI}, rec, nil
}

// serviceScopes validates the requested scopes for a service token.
func serviceScopes(req ServiceRequest) ([]string, error) {
	if len(req.Scopes) == 0 || len(req.Scopes) > 16 {
		return nil, invalid("request between 1 and 16 scopes")
	}
	if req.Audience == TetherAudience {
		out, err := identity.CanonicalScopes(req.Scopes)
		if err != nil {
			return nil, invalid("unknown tether scope")
		}
		for _, s := range out {
			if !slices.Contains(serviceTetherScopes, s) {
				return nil, invalid("a service token may hold only %s", strings.Join(serviceTetherScopes, ", "))
			}
		}
		return out, nil
	}
	seen := map[string]bool{}
	for _, s := range req.Scopes {
		if req.AllowWildcard && s == req.Audience+".*" {
			seen[s] = true
			continue
		}
		m := scopeRe.FindStringSubmatch(s)
		if m == nil || m[1] != req.Audience {
			return nil, invalid("scope %q must be %s.<verb> with an explicit verb", s, req.Audience)
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

// RevokeService revokes one service token by jti. The verifiers that poll the
// deny-list (30 s by default) stop accepting it within that interval; Tether's
// own verifier shares the list and stops at once.
func (i *Issuer) RevokeService(jti string) (ServiceRecord, error) {
	if jti == "" {
		return ServiceRecord{}, invalid("jti required")
	}
	rs, err := i.State.revokeServices(func(r ServiceRecord) bool { return r.JTI == jti })
	if err != nil {
		return ServiceRecord{}, err
	}
	return rs[0], nil
}

// RevokeServicesFor revokes every live service token of one principal
// (msg://service/<app>/<env>).
func (i *Issuer) RevokeServicesFor(app, env string) ([]ServiceRecord, error) {
	if !audRe.MatchString(app) {
		return nil, invalid("app must be one app id")
	}
	if env == "" {
		env = DefaultServiceEnv
	}
	if !envRe.MatchString(env) {
		return nil, invalid("env must be one lowercase id")
	}
	sub := ServicePrincipalID(app, env)
	return i.State.revokeServices(func(r ServiceRecord) bool { return r.Sub == sub })
}
