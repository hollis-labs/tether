package config

import (
	"strings"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
)

// Tether's catalog runtime-kind tokens. They are Tether's own on-disk and
// persisted-plan vocabulary, kept as-is so a catalog and a stored launch plan
// written before the leaf runtimes vocabulary keep working; RuntimeMode is the
// one place they become the libs' runtimes.Mode.
const (
	RuntimeKindPTY            = "pty"
	RuntimeKindStreamingStdio = "streaming-stdio"
	RuntimeKindJSONRPCStdio   = "jsonrpc-stdio"
	RuntimeKindSubprocess     = "subprocess"
	RuntimeKindAPI            = "api"
	RuntimeKindServeHTTP      = "serve-http"
	RuntimeKindPTYDebug       = "pty-debug"
)

// ParseRuntimeKind normalizes a catalog runtime-kind token, accepting the
// aliases older catalogs use ("exec", "app-server", "claude-code", ...).
// It returns "" for a token it does not know. agentkit v0.12.0 removed its
// runtimekind parser and leaves this normalization to each host's boundary.
func ParseRuntimeKind(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "_", "-")
	switch s {
	case "api", "provider-api", "http-api":
		return RuntimeKindAPI
	case "subprocess", "subprocess-per-turn", "exec", "cli", "single-turn", "oneshot", "one-shot":
		return RuntimeKindSubprocess
	case "streaming-stdio", "stream-json", "streaming", "claude-code", "managed-streaming":
		return RuntimeKindStreamingStdio
	case "jsonrpc-stdio", "json-rpc-stdio", "jsonrpc", "app-server", "codex-app-server":
		return RuntimeKindJSONRPCStdio
	case "serve-http", "http-sse", "http", "sse", "opencode-serve":
		return RuntimeKindServeHTTP
	case "pty", "tui", "terminal":
		return RuntimeKindPTY
	case "pty-debug", "debug-pty", "raw-pty":
		return RuntimeKindPTYDebug
	default:
		return ""
	}
}

// RuntimeMode maps a runtime-kind token onto the leaf runtimes.Mode the libs
// take: subprocess is subprocess-per-turn, serve-http is http-sse, pty-debug
// is pty driven under the debug posture (debug=true), and the app-server
// alias is jsonrpc-stdio. api is a direct provider API with no mode, and an
// unknown token has none either; both report ok=false.
func RuntimeMode(kind string) (mode runtimes.Mode, debug bool, ok bool) {
	switch ParseRuntimeKind(kind) {
	case RuntimeKindSubprocess:
		return runtimes.ModeSubprocessPerTurn, false, true
	case RuntimeKindStreamingStdio:
		return runtimes.ModeStreamingStdio, false, true
	case RuntimeKindJSONRPCStdio:
		return runtimes.ModeJSONRPCStdio, false, true
	case RuntimeKindServeHTTP:
		return runtimes.ModeHTTPSSE, false, true
	case RuntimeKindPTY:
		return runtimes.ModePTY, false, true
	case RuntimeKindPTYDebug:
		return runtimes.ModePTY, true, true
	default:
		return "", false, false
	}
}

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
	case p.ID == "antigravity", p.ID == "agy":
		return "antigravity"
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
		if k := ParseRuntimeKind(p.RuntimeKind); k != "" {
			return k
		}
		return p.RuntimeKind
	}
	mode := p.Bootstrap.Mode
	switch mode {
	case "", "agents_md", "prepend":
		// These bootstrap modes describe boot-prompt placement for legacy
		// subprocess providers, not the provider runtime transport.
	default:
		if k := ParseRuntimeKind(mode); k != "" {
			return k
		}
		return mode
	}
	if p.Type == "api" {
		return RuntimeKindAPI
	}
	return RuntimeKindSubprocess
}
