package config

import (
	"strings"

	"github.com/hollis-labs/agentkit/agentruntime/runtimekind"
)

const (
	RuntimeKindPTY            = "pty"
	RuntimeKindStreamingStdio = "streaming-stdio"
	RuntimeKindJSONRPCStdio   = "jsonrpc-stdio"
	RuntimeKindSubprocess     = "subprocess"
	RuntimeKindAPI            = "api"
)

// ProviderBrand returns the catalog provider's adapter/product brand. It
// preserves older catalogs by deriving a brand from adapter/type/id when the
// explicit provider field is absent.
func (p Provider) ProviderBrand() string {
	if p.Provider != "" {
		return p.Provider
	}
	if p.Adapter != "" {
		return p.Adapter
	}
	switch {
	case strings.HasPrefix(p.ID, "claude-"):
		return "claude"
	case strings.HasPrefix(p.ID, "codex-"):
		return "codex"
	case p.ID == "opencode":
		return "opencode"
	case p.ID == "api-stub":
		return "api-stub"
	default:
		return p.ID
	}
}

// EffectiveRuntimeKind returns the provider runtime transport/lifecycle.
// Bootstrap.Mode remains the compatibility source for older catalog records.
func (p Provider) EffectiveRuntimeKind() string {
	if p.RuntimeKind != "" {
		if k := runtimekind.Parse(p.RuntimeKind); k != runtimekind.Unknown {
			return string(k)
		}
		return p.RuntimeKind
	}
	mode := p.Bootstrap.Mode
	switch mode {
	case "", "agents_md", "prepend":
		// These bootstrap modes describe boot-prompt placement for legacy
		// subprocess providers, not the provider runtime transport.
	default:
		switch k := runtimekind.Parse(mode); k {
		case runtimekind.StreamingStdio, runtimekind.JSONRPCStdio, runtimekind.API:
			return string(k)
		case runtimekind.Unknown:
			return mode
		default:
			return string(k)
		}
	}
	if p.Type == "api" {
		return RuntimeKindAPI
	}
	return RuntimeKindSubprocess
}
