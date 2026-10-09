package app

import (
	"fmt"
	"log"

	"github.com/hollis-labs/substrate/harness/adapters"
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	wlaunch "github.com/hollis-labs/substrate/harness/adapters/launch"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	"github.com/hollis-labs/substrate/harness/adapters/runtimebind"
	"github.com/hollis-labs/substrate/llm-core/contracts/runtimes"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/acp"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/provider/cli/antigravity"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
	"github.com/hollis-labs/tether/internal/provider/cli/opencode"
)

// resolvedRuntimeRouting is the one source of runtime/mode confidence claims.
// The publisher's per-output confidence is authoritative. No library currently
// advertises a final-text confidence fact; this claim is useful only after the
// typed output publisher is installed (CW-0062).
func resolvedRuntimeRouting(p config.Provider) (string, string, bool, error) {
	if _, err := runtimeFactoryForProvider(p); err != nil {
		return "", "unknown", false, err
	}
	mode, debug, ok := config.RuntimeMode(p.EffectiveRuntimeKind())
	if !ok {
		return "", "unknown", false, fmt.Errorf("no routing mode")
	}
	req := runtimebind.Request{Provider: p.ProviderBrand(), RequestedRuntime: mode, AllowPTY: true}
	if debug {
		req.Posture = runtimebind.PostureDebug
	}
	binding, err := runtimebind.Resolve(req)
	if err != nil {
		return "", "unknown", false, err
	}
	confidence := "unknown"
	if binding.Runtime.ACP() {
		confidence = "heuristic"
	} else {
		switch binding.Provider {
		case "claude", "codex", "antigravity":
			if binding.Runtime != runtimes.ModePTY {
				confidence = "exact"
			}
		case "opencode":
			if binding.Runtime == runtimes.ModeSubprocessPerTurn {
				confidence = "exact"
			}
			if binding.Runtime == runtimes.ModeHTTPSSE {
				confidence = "none"
			}
		}
	}
	sel := wlaunch.Selection{Runtime: binding.Provider, Mode: binding.Runtime}
	// Native Tether adapters are plan-scoped before reaching agentsessions.
	// Query that actual wrapper shape, which may hide optional interfaces.
	var cli gop.CLIAdapter
	switch {
	case binding.Provider == "claude" && binding.Runtime == runtimes.ModeStreamingStdio:
		cli = gop.NewClaudeAdapterStreamingStdio()
	case binding.Provider == "claude" && binding.Runtime == runtimes.ModeSubprocessPerTurn:
		cli = gop.NewClaudeAdapter()
	case binding.Provider == "codex" && binding.Runtime == runtimes.ModeJSONRPCStdio:
		cli = gop.NewCodexAdapterAppServer()
	case binding.Provider == "codex" && binding.Runtime == runtimes.ModeSubprocessPerTurn:
		cli = gop.NewCodexAdapter()
	}
	if cli != nil {
		sel.CLIAdapter = claudestream.NewPlanScopedAdapter(nil, cli)
	}
	adapter, err := wlaunch.Select(sel)
	if err != nil {
		return binding.Provider, "unknown", false, nil //nolint:nilerr // Valid host modes such as PTY have no wrapper delivery descriptor.
	}
	return binding.Provider, confidence, adapter.Describe().Delivery.Supports(adapters.DeliveryCapabilityCancelTurn), nil
}

func runtimeFactoryForProvider(p config.Provider) (RuntimeFactory, error) {
	brand := p.ProviderBrand()
	runtimeKind := p.EffectiveRuntimeKind()
	if brand == "api-stub" && runtimeKind == config.RuntimeKindAPI {
		return stub.New, nil
	}
	// The catalog's runtime-kind token becomes a leaf mode here, at the lib
	// boundary (config.RuntimeMode). An explicit mode also keeps a catalog
	// codex provider on the mode it names: the registry default for codex is
	// now jsonrpc-stdio, so an unset mode would silently switch exec
	// providers to app-server.
	mode, debug, ok := config.RuntimeMode(runtimeKind)
	if !ok {
		return nil, fmt.Errorf("unsupported provider/runtime_kind combination: provider=%q runtime_kind=%q", brand, runtimeKind)
	}
	req := runtimebind.Request{
		Provider:         brand,
		RequestedRuntime: mode,
		AllowPTY:         true,
	}
	if debug {
		req.Posture = runtimebind.PostureDebug
	}
	binding, err := runtimebind.Resolve(req)
	if err != nil {
		return nil, err
	}

	switch {
	case binding.Provider == "claude" && binding.Runtime == runtimes.ModeStreamingStdio:
		return newClaudeStreamingStdioRuntime(p.ID), nil
	case binding.Provider == "claude" && binding.Runtime == runtimes.ModePTY:
		// Deprecated: PTY is not the user-facing runtime going forward (D7, ADR 0044).
		// subprocess and streaming-stdio are the primary runtimes. PTY is not removed
		// yet — marker-only until output-capture is fully complete.
		log.Printf("WARN: resolving deprecated PTY runtime for provider %q; prefer streaming_stdio or subprocess", p.ID)
		return newClaudePTYRuntime(p.ID), nil
	case binding.Provider == "claude" && binding.Runtime == runtimes.ModeSubprocessPerTurn:
		return newGoproviderRuntime(p.ID, gop.NewClaudeAdapter(), agentsessions.Capabilities{
			ProviderSessionID: true,
			BinaryRequired:    true,
		}), nil
	case binding.Provider == "codex" && binding.Runtime == runtimes.ModeJSONRPCStdio:
		return newCodexJSONRPCStdioRuntime(p.ID), nil
	case binding.Provider == "codex" && binding.Runtime == runtimes.ModeSubprocessPerTurn:
		return newGoproviderRuntime(p.ID, gop.NewCodexAdapter(), agentsessions.Capabilities{
			BinaryRequired:    true,
			ProviderSessionID: true,
		}), nil
	case binding.Provider == "opencode" && binding.Runtime == runtimes.ModeSubprocessPerTurn:
		return opencode.New, nil
	case binding.Provider == "antigravity" && binding.Runtime == runtimes.ModeSubprocessPerTurn:
		return antigravity.New, nil
	case binding.Runtime.ACP():
		// Any registry runtime driven over ACP (Copilot, Pi), launched
		// through go-agent-wrapper's launch.Select (CW-20260930-0106 stage
		// 1). A new ACP descriptor in the registry needs no case here.
		return newACPRuntime(p.ID, binding.Provider, binding.Runtime), nil
	default:
		return nil, fmt.Errorf("unsupported provider/runtime_kind combination: provider=%q runtime_kind=%q", brand, runtimeKind)
	}
}

func newClaudeStreamingStdioRuntime(providerID string) RuntimeFactory {
	return func(plan *launch.Plan) (agentsessions.Runtime, error) {
		adapter := gop.NewClaudeAdapterStreamingStdio()
		adapter.ApiKeyHelperPath = resolveAPIKeyHelperPath()
		return claudestream.NewWithAdapter(plan, adapter, providerID, agentsessions.Capabilities{
			StreamingStdio:    true,
			ProviderSessionID: true,
			CheckpointResume:  false,
			BinaryRequired:    true,
		})
	}
}

func newClaudePTYRuntime(providerID string) RuntimeFactory {
	return func(plan *launch.Plan) (agentsessions.Runtime, error) {
		adapter := gop.NewClaudeAdapterPTY()
		adapter.ApiKeyHelperPath = resolveAPIKeyHelperPath()
		return claudestream.NewWithAdapter(plan, adapter, providerID, agentsessions.Capabilities{
			PTY:               true,
			Resize:            true,
			ProviderSessionID: true,
			CheckpointResume:  false,
			BinaryRequired:    true,
		})
	}
}

func newCodexJSONRPCStdioRuntime(providerID string) RuntimeFactory {
	return func(plan *launch.Plan) (agentsessions.Runtime, error) {
		return claudestream.NewWithAdapter(plan, gop.NewCodexAdapterAppServer(), providerID, agentsessions.Capabilities{
			JsonRpcStdio:      true,
			ProviderSessionID: true,
			CheckpointResume:  false,
			BinaryRequired:    true,
		})
	}
}

func newACPRuntime(providerID, runtimeID string, mode runtimes.Mode) RuntimeFactory {
	return func(plan *launch.Plan) (agentsessions.Runtime, error) {
		return acp.New(providerID, runtimeID, mode, plan.Command)
	}
}

func newGoproviderRuntime(providerID string, adapter gop.CLIAdapter, caps agentsessions.Capabilities) RuntimeFactory {
	return func(plan *launch.Plan) (agentsessions.Runtime, error) {
		return claudestream.NewWithAdapter(plan, adapter, providerID, caps)
	}
}
