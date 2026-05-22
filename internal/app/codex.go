package app

import (
	"context"
	"encoding/json"
	"fmt"
)

// muxClientVersion is the value reported in the JSON-RPC initialize
// clientInfo field. It's a build-time constant rather than wired from
// the binary's version (no version package exists yet); the value is
// used only for diagnostic identification by the Codex app-server.
const muxClientVersion = "v005-07"

// sendTurnJSONRPC implements the JSON-RPC turn delivery for codex
// app-server style runtimes. Routes through Manager.JsonRpcCall (added
// in go-agent-sessions v0.9.0) so the raw Session reference stays
// hidden behind the Manager surface. Lazily runs initialize +
// thread/start on the first call for a session, caches the thread id,
// and issues turn/start with the cached id + user input.
func (s *Service) sendTurnJSONRPC(ctx context.Context, id, text string) error {
	threadID, cached := s.codexThreads.Load(id)
	if !cached {
		initParams := map[string]any{
			"clientInfo": map[string]any{
				"name":    "agent-mux",
				"version": muxClientVersion,
			},
		}
		if _, err := s.Manager.JsonRpcCall(ctx, id, "initialize", initParams); err != nil {
			return fmt.Errorf("jsonrpc initialize: %w", err)
		}
		startRes, err := s.Manager.JsonRpcCall(ctx, id, "thread/start", s.codexThreadStartParams(id))
		if err != nil {
			return fmt.Errorf("jsonrpc thread/start: %w", err)
		}
		var parsed struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(startRes, &parsed); err != nil {
			return fmt.Errorf("decode thread/start response: %w", err)
		}
		if parsed.Thread.ID == "" {
			return fmt.Errorf("thread/start returned empty thread.id")
		}
		threadID = parsed.Thread.ID
		s.codexThreads.Store(id, threadID)
	}
	if _, err := s.Manager.JsonRpcCall(ctx, id, "turn/start", map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{"type": "text", "text": text},
		},
	}); err != nil {
		return fmt.Errorf("jsonrpc turn/start: %w", err)
	}
	return nil
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
