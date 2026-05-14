package config

import "strings"

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
		return p.RuntimeKind
	}
	switch p.Bootstrap.Mode {
	case RuntimeKindStreamingStdio, RuntimeKindJSONRPCStdio:
		return p.Bootstrap.Mode
	}
	if p.Type == "api" {
		return RuntimeKindAPI
	}
	return RuntimeKindSubprocess
}
