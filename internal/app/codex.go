package app

import (
	"context"
	"encoding/json"

	"github.com/hollis-labs/go-agent-runtime/turn"
)

// muxClientVersion is the value reported in the JSON-RPC initialize
// clientInfo field. It's a build-time constant rather than wired from
// the binary's version (no version package exists yet); the value is
// used only for diagnostic identification by the Codex app-server.
const muxClientVersion = "v005-07"

// sendTurnJSONRPC implements Codex app-server turn delivery through the shared
// go-agent-runtime protocol helper. Tether still owns the session lookup and
// work-root projection; the shared layer owns initialize/thread/start caching
// and turn/start framing.
func (s *Service) sendTurnJSONRPC(ctx context.Context, id, text string) error {
	return s.codexThreads.SendTurn(ctx, id, managerJSONRPCSender{s: s, id: id}, text, s.codexAppServerOptions(id))
}

func (s *Service) codexThreadStartParams(sessionID string) map[string]any {
	params := map[string]any{}
	if s == nil || s.Store == nil {
		return params
	}
	plan, err := s.Store.GetLaunchPlan(sessionID)
	if err != nil || plan == nil {
		return params
	}
	if cwd := plan.EffectiveWorkRoot(); cwd != "" {
		params["cwd"] = cwd
	}
	return params
}

func (s *Service) codexAppServerOptions(sessionID string) turn.CodexAppServerOptions {
	opts := turn.CodexAppServerOptions{
		ClientName:    "agent-mux",
		ClientVersion: muxClientVersion,
	}
	if cwd, ok := s.codexThreadStartParams(sessionID)["cwd"].(string); ok {
		opts.CWD = cwd
	}
	return opts
}

type managerJSONRPCSender struct {
	s  *Service
	id string
}

func (m managerJSONRPCSender) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return m.s.Manager.JsonRpcCall(ctx, m.id, method, params)
}
