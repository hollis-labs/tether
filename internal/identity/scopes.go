package identity

import (
	"fmt"
	"sort"
)

const (
	ScopeRead     = "read"
	ScopeOperate  = "operate"
	ScopeTerminal = "terminal"
	ScopeMaintain = "maintain"
	ScopeAdmin    = "admin"
)

// NormalizeDeviceScopes validates independent capabilities, not a privilege hierarchy.
// In particular admin does not imply terminal, operate or maintain, and legacy
// wildcards do not become remote device authority (ADR 0062).
func NormalizeDeviceScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 || len(scopes) > 5 {
		return nil, fmt.Errorf("choose at least one of read, operate, terminal, maintain, admin")
	}
	seen := make(map[string]bool)
	for _, scope := range scopes {
		switch scope {
		case ScopeRead, ScopeOperate, ScopeTerminal, ScopeMaintain, ScopeAdmin:
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
