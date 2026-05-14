package app

import (
	"fmt"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
	"github.com/hollis-labs/tether/internal/provider/cli/opencode"
)

func runtimeFactoryForProvider(p config.Provider) (RuntimeFactory, error) {
	brand := p.ProviderBrand()
	runtimeKind := p.EffectiveRuntimeKind()

	switch {
	case brand == "api-stub" && runtimeKind == config.RuntimeKindAPI:
		return stub.New, nil
	case brand == "claude" && runtimeKind == config.RuntimeKindStreamingStdio:
		return newClaudeStreamingStdioRuntime(p.ID), nil
	case brand == "claude" && runtimeKind == config.RuntimeKindPTY:
		return newClaudePTYRuntime(p.ID), nil
	case brand == "claude" && runtimeKind == config.RuntimeKindSubprocess:
		return newGoproviderRuntime(p.ID, gop.NewClaudeAdapter(), agentsessions.Capabilities{
			ProviderSessionID: true,
			BinaryRequired:    true,
		}), nil
	case brand == "codex" && runtimeKind == config.RuntimeKindJSONRPCStdio:
		return newCodexJSONRPCStdioRuntime(p.ID), nil
	case brand == "codex" && runtimeKind == config.RuntimeKindSubprocess:
		return newGoproviderRuntime(p.ID, gop.NewCodexAdapter(), agentsessions.Capabilities{
			BinaryRequired: true,
		}), nil
	case brand == "opencode" && runtimeKind == config.RuntimeKindSubprocess:
		return opencode.New, nil
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
			JsonRpcStdio:     true,
			CheckpointResume: false,
			BinaryRequired:   true,
		})
	}
}

func newGoproviderRuntime(providerID string, adapter gop.CLIAdapter, caps agentsessions.Capabilities) RuntimeFactory {
	return func(plan *launch.Plan) (agentsessions.Runtime, error) {
		return claudestream.NewWithAdapter(plan, adapter, providerID, caps)
	}
}
