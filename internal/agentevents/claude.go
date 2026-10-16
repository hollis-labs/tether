package agentevents

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

// ClaudeHook maps one Claude Code hook payload (the JSON a command or HTTP hook receives on stdin) to the observations it
// stands for. It is a pure function of the payload: no I/O, no state, no clock. An event it does not know, or a Notification
// type it does not map, yields no observations and no error, so a new Claude release never breaks a hook.
//
// What goes into a payload is deliberately small and never carries content: the prompt text, tool inputs and tool results can
// hold secrets and source code, and nothing here has been through the sanitize stage yet (CW-20261016-0144). The adapter
// records the tool name, the tool-use id, sizes, and the ids that join an event to its hook peers; the vendor's transcript
// path remains the place to read content, under the user's own file permissions.
//
// Tier is Exact for every mapping: the hook fired because the thing happened. The stateful joins (a tool result's ParentID,
// a turn's id) need the earlier event's id and belong to the adapter that holds the session, not to this function.
func ClaudeHook(payload []byte) ([]Observation, error) {
	var p claudeHookPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("agentevents: claude hook payload: %w", err)
	}
	if p.Event == "" {
		return nil, errors.New("agentevents: claude hook payload has no hook_event_name")
	}
	hook := func(kind runtimeevents.EventKind, payload any) Observation {
		return Observation{Kind: kind, Channel: runtimeevents.ChannelHook, Tier: Exact, Payload: payload}
	}
	switch p.Event {
	case "SessionStart":
		return []Observation{hook(runtimeevents.KindSessionReady, map[string]any{"source": p.Source, "model": p.Model})}, nil
	case "UserPromptSubmit":
		return []Observation{
			hook(runtimeevents.KindTurnStarted, map[string]any{"prompt_bytes": len(p.Prompt)}),
			hook(runtimeevents.KindSessionProcessing, nil),
		}, nil
	case "PreToolUse":
		return []Observation{hook(runtimeevents.KindAgentToolUse, map[string]any{"tool": p.ToolName, "tool_use_id": p.ToolUseID, "input_bytes": len(p.ToolInput)})}, nil
	case "PostToolUse":
		return []Observation{hook(runtimeevents.KindAgentToolResult, map[string]any{"tool": p.ToolName, "tool_use_id": p.ToolUseID, "ok": true, "result_bytes": len(p.ToolResponse)})}, nil
	case "PostToolUseFailure":
		return []Observation{hook(runtimeevents.KindAgentToolResult, map[string]any{"tool": p.ToolName, "tool_use_id": p.ToolUseID, "ok": false})}, nil
	case "PermissionRequest":
		return []Observation{hook(runtimeevents.KindAgentPermissionRequested, map[string]any{"tool": p.ToolName, "tool_use_id": p.ToolUseID, "ask": "permission"})}, nil
	case "PermissionDenied":
		return []Observation{hook(runtimeevents.KindAgentPermissionDenied, map[string]any{"tool": p.ToolName, "tool_use_id": p.ToolUseID})}, nil
	case "Notification":
		switch p.NotificationType {
		case "permission_prompt":
			return []Observation{hook(runtimeevents.KindAgentPermissionRequested, map[string]any{"ask": "permission"})}, nil
		case "elicitation_dialog":
			return []Observation{hook(runtimeevents.KindAgentPermissionRequested, map[string]any{"ask": "user_input"})}, nil
		case "idle_prompt":
			return []Observation{hook(runtimeevents.KindSessionIdle, nil)}, nil
		}
		return nil, nil
	case "SubagentStart":
		return []Observation{hook(runtimeevents.KindAgentSubagentSpawn, map[string]any{"agent_id": p.AgentID, "agent_type": p.AgentType})}, nil
	case "SubagentStop":
		// No stable catalog kind says a subagent ended; a turn.completed labeled "subagent" carries it until one exists.
		o := hook(runtimeevents.KindTurnCompleted, map[string]any{"agent_id": p.AgentID, "agent_type": p.AgentType})
		o.Labels = []string{"subagent"}
		return []Observation{o}, nil
	case "Stop":
		return []Observation{hook(runtimeevents.KindTurnCompleted, nil), hook(runtimeevents.KindSessionIdle, nil)}, nil
	case "SessionEnd":
		return []Observation{hook(runtimeevents.KindProcessExited, map[string]any{"reason": p.Reason})}, nil
	}
	return nil, nil
}

// ClaudeSessionID is the vendor session id a hook payload carries, for the adapter that maps it to a Tether session.
func ClaudeSessionID(payload []byte) string {
	var p struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.SessionID
}

type claudeHookPayload struct {
	Event            string          `json:"hook_event_name"`
	SessionID        string          `json:"session_id"`
	Source           string          `json:"source"`
	Model            string          `json:"model"`
	Prompt           string          `json:"prompt"`
	ToolName         string          `json:"tool_name"`
	ToolUseID        string          `json:"tool_use_id"`
	ToolInput        json.RawMessage `json:"tool_input"`
	ToolResponse     json.RawMessage `json:"tool_response"`
	NotificationType string          `json:"notification_type"`
	AgentID          string          `json:"agent_id"`
	AgentType        string          `json:"agent_type"`
	Reason           string          `json:"reason"`
}
