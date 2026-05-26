package app

import (
	"fmt"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentruntime/runtimebind"
	"github.com/hollis-labs/agentkit/agentsessions"
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
	if brand == "api-stub" && runtimeKind == config.RuntimeKindAPI {
		return stub.New, nil
	}
	binding, err := runtimebind.Resolve(runtimebind.Request{
		Provider:         brand,
		RequestedRuntime: agentlaunch.RuntimeKind(runtimeKind),
		AllowPTY:         true,
	})
	if err != nil {
		return nil, err
	}

	switch {
	case binding.Provider == "claude" && binding.Runtime == agentlaunch.RuntimeStreamingStdio:
		return newClaudeStreamingStdioRuntime(p.ID), nil
	case binding.Provider == "claude" && binding.Runtime == agentlaunch.RuntimePTY:
		return newClaudePTYRuntime(p.ID), nil
	case binding.Provider == "claude" && binding.Runtime == agentlaunch.RuntimeSubprocess:
		return newGoproviderRuntime(p.ID, gop.NewClaudeAdapter(), agentsessions.Capabilities{
			ProviderSessionID: true,
			BinaryRequired:    true,
		}), nil
	case binding.Provider == "codex" && binding.Runtime == agentlaunch.RuntimeJsonRpcStdio:
		return newCodexJSONRPCStdioRuntime(p.ID), nil
	case binding.Provider == "codex" && binding.Runtime == agentlaunch.RuntimeSubprocess:
		return newGoproviderRuntime(p.ID, gop.NewCodexAdapter(), agentsessions.Capabilities{
			BinaryRequired: true,
		}), nil
	case binding.Provider == "opencode" && binding.Runtime == agentlaunch.RuntimeSubprocess:
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
