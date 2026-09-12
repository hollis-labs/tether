package app

import (
	"encoding/json"
	"testing"
)

// realElicitationRequest is the params block codex-cli 0.154.0 actually
// sent for an MCP tool-call approval, captured from a live
// agent-mux-codex-app-server session on 2026-09-12. Kept verbatim so this
// test breaks if the shape Tether keys on (`_meta.codex_approval_kind`)
// moves, rather than asserting against a hand-written approximation of it.
const realElicitationRequest = `{
  "threadId": "01a095f4-371e-7992-8cdb-7904962af41f",
  "turnId": "01a095f4-3750-78a2-a722-ebd832593249",
  "serverName": "mux",
  "mode": "form",
  "_meta": {
    "codex_approval_kind": "mcp_tool_call",
    "persist": ["session", "always"],
    "tool_description": "Health check for the agent-mux MCP adapter.",
    "tool_params": {},
    "tool_params_display": []
  },
  "message": "Allow the mux MCP server to run tool \"mux_health\"?",
  "requestedSchema": {"type": "object", "properties": {}}
}`

func TestJSONRPCRequestHookApprovesMCPToolCall(t *testing.T) {
	hook := jsonRPCRequestHook("sess-1")
	result, rpcErr := hook(codexElicitationMethod, json.RawMessage(realElicitationRequest))
	if rpcErr != nil {
		t.Fatalf("approval refused: %v", rpcErr)
	}
	accept, ok := result.(elicitationAccept)
	if !ok {
		t.Fatalf("result type = %T, want elicitationAccept", result)
	}
	if accept.Action != elicitationActionAccept {
		t.Errorf("action = %q, want %q", accept.Action, elicitationActionAccept)
	}
	// Codex asks for an object-shaped response even when requestedSchema is
	// empty. A nil map marshals to `null`, not `{}`, so assert on the wire
	// form rather than on the Go value.
	encoded, err := json.Marshal(accept)
	if err != nil {
		t.Fatalf("marshal accept: %v", err)
	}
	if got, want := string(encoded), `{"action":"accept","content":{}}`; got != want {
		t.Errorf("wire form = %s, want %s", got, want)
	}
}

// A non-approval request must keep agentkit's fail-fast behavior. Returning
// (nil, nil) here would marshal to a successful `null` result, which codex
// reads as consent — so the hook must return an explicit error instead.
func TestJSONRPCRequestHookRefusesUnknownRequests(t *testing.T) {
	hook := jsonRPCRequestHook("sess-1")
	cases := []struct {
		name   string
		method string
		params string
	}{
		{
			name:   "unrelated method",
			method: "item/commandExecution/requestApproval",
			params: `{}`,
		},
		{
			name:   "elicitation for a future approval kind",
			method: codexElicitationMethod,
			params: `{"serverName":"mux","_meta":{"codex_approval_kind":"exec_escalation"}}`,
		},
		{
			name:   "elicitation with no approval kind at all",
			method: codexElicitationMethod,
			params: `{"serverName":"mux"}`,
		},
		{
			name:   "malformed params for a handled method",
			method: codexElicitationMethod,
			params: `{"_meta": "not-an-object"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, rpcErr := hook(tc.method, json.RawMessage(tc.params))
			if rpcErr == nil {
				t.Fatalf("got approval (result %v), want an error", result)
			}
			if result != nil {
				t.Errorf("result = %v, want nil alongside the error", result)
			}
		})
	}
}
