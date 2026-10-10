package environment

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	SessionCore = "session_core"
	LocalMCP    = "local_mcp"
	Messaging   = "messaging"
	// #nosec G101 -- module name, not credential material.
	CredentialBroker     = "credential_broker"
	RemoteListener       = "remote_listener"
	StreamAPI            = "stream_api"
	Lifecycle            = "lifecycle"
	EnvironmentDirectory = "environment_directory"
	ConnectionManager    = "connection_manager"
	Teams                = "teams"
	LLMGateway           = "llm_gateway"
)

var moduleNames = []string{SessionCore, LocalMCP, Messaging, CredentialBroker, RemoteListener, StreamAPI, Lifecycle, EnvironmentDirectory, ConnectionManager, Teams, LLMGateway}

// Profile is an immutable startup policy. It selects exposed modules, never
// enrolls actors or changes authority over existing sessions and deliveries.
type Profile struct {
	Role    string
	Legacy  bool
	enabled map[string]bool
}

// ResolveProfile preserves the existing opt-ins when role is omitted. An
// explicit hub includes worker features; a worker defaults hub features off.
func ResolveProfile(role string, overrides map[string]bool, legacyTeams, legacyAI bool) (*Profile, error) {
	p := &Profile{Role: role, Legacy: role == "", enabled: make(map[string]bool)}
	if role == "" {
		p.Role = "hub"
	}
	if p.Role != "worker" && p.Role != "hub" {
		return nil, fmt.Errorf("role must be worker or hub")
	}
	for _, name := range moduleNames {
		p.enabled[name] = true
	}
	for _, name := range []string{EnvironmentDirectory, ConnectionManager, Teams, LLMGateway} {
		p.enabled[name] = p.Role == "hub"
	}
	if p.Legacy {
		p.enabled[Teams], p.enabled[LLMGateway] = legacyTeams, legacyAI
		// These future hub modules did not exist in the legacy composition.
		p.enabled[EnvironmentDirectory], p.enabled[ConnectionManager] = false, false
	}
	for name, on := range overrides {
		if _, known := p.enabled[name]; !known {
			return nil, fmt.Errorf("unknown module %q", name)
		}
		p.enabled[name] = on
	}
	return p, nil
}

func (p *Profile) Enabled(name string) bool { return p == nil || p.enabled[name] }
func ModuleNames() []string                 { return append([]string(nil), moduleNames...) }

// ModuleGate hides disabled surfaces after authentication. Durable work already
// accepted by the local service keeps its existing recovery/drain semantics.
func (p *Profile) ModuleGate(next http.Handler) http.Handler {
	if p == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		module := pathModule(r.URL.Path)
		lifecycle := strings.HasPrefix(r.URL.Path, "/sessions") && r.Method != http.MethodGet && r.Method != http.MethodHead
		if (module != "" && !p.Enabled(module)) || (lifecycle && !p.Enabled(Lifecycle)) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func pathModule(path string) string {
	first := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
	switch first {
	case "mcp", "p":
		return LocalMCP
	case "ai":
		return LLMGateway
	case "teams":
		return Teams
	case "messages", "channels", "groups", "mentions", "routing", "registry", "a2a":
		return Messaging
	case "broker":
		return CredentialBroker
	case "environment":
		return StreamAPI
	case "sessions":
		if strings.HasSuffix(path, "/snapshot") || strings.HasSuffix(path, "/stream") {
			return StreamAPI
		}
		return SessionCore
	case "session-groups", "logical-agents", "workstreams", "checkpoints", "events":
		return SessionCore
	default:
		return ""
	}
}
