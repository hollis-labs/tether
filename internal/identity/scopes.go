package identity

import (
	"fmt"
	"sort"
	"strings"
)

const (
	ScopeRead     = "read"
	ScopeOperate  = "operate"
	ScopeTerminal = "terminal"
	ScopeMaintain = "maintain"
	ScopeAdmin    = "admin"
	ScopeWorker   = "worker"
)

// NormalizeDeviceScopes validates independent capabilities, not a privilege hierarchy.
// In particular admin does not imply terminal, operate or maintain, and legacy
// wildcards do not become remote device authority (ADR 0062).
func NormalizeDeviceScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 || len(scopes) > 6 {
		return nil, fmt.Errorf("choose at least one of read, operate, terminal, maintain, admin, worker")
	}
	seen := make(map[string]bool)
	for _, scope := range scopes {
		switch scope {
		case ScopeRead, ScopeOperate, ScopeTerminal, ScopeMaintain, ScopeAdmin, ScopeWorker:
		default:
			return nil, fmt.Errorf("unknown device scope")
		}
		seen[scope] = true
	}
	out := make([]string, 0, len(seen))
	for scope := range seen {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out, nil
}

func HasDeviceScope(p Principal, scope string) bool {
	if p.Kind != "device" || p.ID == "" || p.ID == OperatorID {
		return false
	}
	for _, granted := range p.Scopes {
		if granted == scope {
			return true
		}
	}
	return false
}

func NarrowDeviceScopes(granted, requested []string) ([]string, error) {
	if requested == nil {
		requested = granted
	}
	scopes, err := NormalizeDeviceScopes(requested)
	if err != nil {
		return nil, err
	}
	for _, scope := range scopes {
		found := false
		for _, grant := range granted {
			if scope == grant {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("requested scope not granted")
		}
	}
	return scopes, nil
}

// AppScopePrefix namespaces Tether's scopes in the cross-app vocabulary
// (<app>.<verb>). The five device scopes plus worker map 1:1: read is
// tether.read, and so on. tth_ device credentials keep storing the bare names;
// issued JWTs carry the namespaced ones; the two are interchangeable wherever
// scope is checked (ScopeGranted).
const AppScopePrefix = "tether."

// CanonicalScope returns the namespaced form of a bare device scope; any other
// string is returned unchanged.
func CanonicalScope(s string) string {
	switch s {
	case ScopeRead, ScopeOperate, ScopeTerminal, ScopeMaintain, ScopeAdmin, ScopeWorker:
		return AppScopePrefix + s
	}
	return s
}

// IsCanonicalScope reports whether s is one of the six tether.* scopes.
func IsCanonicalScope(s string) bool {
	rest, ok := strings.CutPrefix(s, AppScopePrefix)
	return ok && CanonicalScope(rest) == s
}

// legacyScopeNeeds lists, for each scope the worker/MCP child credentials used
// before the unified vocabulary (CW-20261014-0035), the canonical scopes that
// satisfy a check for it. The mapping is deliberately one-directional: a
// canonical/device credential satisfies a legacy check, but a legacy
// credential does not gain canonical authority.
var legacyScopeNeeds = map[string]string{
	"session.write":  AppScopePrefix + ScopeOperate,
	"message.write":  AppScopePrefix + ScopeOperate,
	"groups.write":   AppScopePrefix + ScopeOperate,
	"team.write":     AppScopePrefix + ScopeOperate,
	"ai.invoke":      AppScopePrefix + ScopeOperate,
	"registry.write": AppScopePrefix + ScopeMaintain,
	"catalog.write":  AppScopePrefix + ScopeMaintain,
}

// IsLegacyChildScope reports whether s is one of the pre-unification MCP child scopes.
func IsLegacyChildScope(s string) bool { _, ok := legacyScopeNeeds[s]; return ok }

// ScopeGranted reports whether granted satisfies required. required may be a
// bare device scope, a tether.* scope or a legacy child scope. "*" and an
// "<app>.*" wildcard grant everything in that scope space.
func ScopeGranted(granted []string, required string) bool {
	want := CanonicalScope(required)
	if need, ok := legacyScopeNeeds[required]; ok {
		for _, g := range granted {
			if g == required {
				return true
			}
		}
		want = need
	}
	for _, g := range granted {
		if g == "*" || CanonicalScope(g) == want {
			return true
		}
		if pre, ok := strings.CutSuffix(g, ".*"); ok && strings.HasPrefix(want, pre+".") {
			return true
		}
	}
	return false
}

// CanonicalScopes maps any mix of bare device scopes, tether.* scopes and
// legacy child scopes to a sorted, de-duplicated tether.* set. Unknown scopes
// are an error: nothing outside the vocabulary is ever minted.
func CanonicalScopes(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, s := range in {
		switch {
		case IsLegacyChildScope(s):
			seen[legacyScopeNeeds[s]] = true
		case IsCanonicalScope(s):
			seen[s] = true
		case CanonicalScope(s) != s:
			seen[CanonicalScope(s)] = true
		default:
			return nil, fmt.Errorf("unknown scope")
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// ScopesCover reports whether granted satisfies every scope in requested.
func ScopesCover(granted, requested []string) bool {
	for _, r := range requested {
		if !ScopeGranted(granted, r) {
			return false
		}
	}
	return true
}
