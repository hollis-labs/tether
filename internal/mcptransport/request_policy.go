package mcptransport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

type selectors struct {
	profiles, modes []mcpgateway.Selector
}

func requestSelectors(r *http.Request) (selectors, error) {
	s := selectors{}
	if r.URL.Path != "/mcp" {
		id, ok := strings.CutPrefix(r.URL.Path, "/p/")
		if !ok || id == "" || strings.Contains(id, "/") {
			return s, fmt.Errorf("invalid MCP profile path")
		}
		s.profiles = append(s.profiles, mcpgateway.Selector{Value: id, Source: "path"})
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return s, fmt.Errorf("invalid MCP query")
	}
	for _, key := range []string{"token", "access_token", "bearer"} {
		if _, present := query[key]; present {
			return s, fmt.Errorf("query credentials are forbidden")
		}
	}
	for _, source := range []struct {
		values []string
		name   string
		target *[]mcpgateway.Selector
	}{{query["profile"], "query", &s.profiles}, {r.Header.Values("X-Tether-Profile"), "header", &s.profiles}, {query["discovery_mode"], "query", &s.modes}, {r.Header.Values("X-Tether-Discovery-Mode"), "header", &s.modes}} {
		for _, value := range source.values {
			*source.target = append(*source.target, mcpgateway.Selector{Value: value, Source: source.name})
		}
	}
	return s, nil
}

func viewFingerprint(caller Caller, token string, opts mcpadapter.ProxyOptions) (string, error) {
	mode, err := mcpgateway.ResolveMode(opts.ModeInputs)
	if err != nil {
		return "", err
	}
	scopes := slices.Clone(caller.Principal.Scopes)
	slices.Sort(scopes)
	// Selector source labels are presentation; resolved content is authority.
	payload := struct {
		PrincipalID, Kind, SessionID, TokenHash, PolicyDigest string
		Scopes                                                []string
		Profile                                               *mcpgateway.Profile
		Mode                                                  mcpgateway.Mode
	}{caller.Principal.ID, caller.Principal.Kind, caller.Principal.SessionID, identity.HashToken(token), caller.Policy.Digest, scopes, opts.Profile.Profile, mode.Mode}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:]), nil
}

// authorityPolicy checks the actual Host, never forwarding headers. UDS has
// no network authority: only the client's synthetic unix/localhost names work.
type authorityPolicy struct{ allowed map[string]bool }

func newAuthorityPolicy(addr string) (authorityPolicy, error) {
	p := authorityPolicy{allowed: map[string]bool{}}
	if strings.HasPrefix(addr, "unix:") {
		for _, host := range []string{"unix", "localhost", "127.0.0.1", "[::1]"} {
			p.allowed[host] = true
		}
		return p, nil
	}
	if !strings.HasPrefix(addr, "tcp:") {
		return p, fmt.Errorf("MCP requires unix: or tcp: listener")
	}
	authority := strings.TrimPrefix(addr, "tcp:")
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port == "0" || host == "" {
		return p, fmt.Errorf("MCP requires a concrete listener authority")
	}
	p.allowed[normalizeAuthority(authority)] = true
	if host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		for _, alias := range []string{"localhost", "127.0.0.1", "::1"} {
			p.allowed[normalizeAuthority(net.JoinHostPort(alias, port))] = true
		}
	}
	return p, nil
}

func normalizeAuthority(host string) string {
	host = strings.ToLower(host)
	if name, port, err := net.SplitHostPort(host); err == nil && port == "80" {
		if strings.Contains(name, ":") {
			return "[" + name + "]"
		}
		return name
	}
	return host
}

func (p authorityPolicy) validate(r *http.Request) bool {
	if !p.allowed[normalizeAuthority(r.Host)] {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && origin.Scheme == scheme && origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" && normalizeAuthority(origin.Host) == normalizeAuthority(r.Host)
}

func selectedUpstreamOrigins(a admission) ([]string, error) {
	known := map[string]bool{"tether": true}
	for id, enabled := range a.catalog.MCPServerEnabled {
		known[id] = enabled
	}
	selected := append([]string{}, a.options.ServerFilter...)
	for _, floor := range a.options.AuthorityProfiles {
		var err error
		selected, err = mcpgateway.SelectOrigins(known, selected, floor.Profile)
		if err != nil {
			return nil, err
		}
	}
	selected, err := mcpgateway.SelectOrigins(known, selected, a.options.Profile.Profile)
	if err != nil {
		return nil, err
	}
	origins := []string{}
	for _, id := range selected {
		if id != "tether" {
			origins = append(origins, id)
		}
	}
	return origins, nil
}

// ValidateEndpoint is used at daemon startup and doctor, never by shared loaders.
func ValidateEndpoint(addr string, mode identity.Mode) error {
	if err := identity.ValidateBind(addr, mode); err != nil {
		return err
	}
	_, err := newAuthorityPolicy(addr)
	return err
}
