package app

// codex_approval.go — answering codex app-server's server-initiated
// approval requests.
//
// Why this exists: codex app-server does not just stream events at its
// client. For anything it considers approval-gated — currently MCP tool
// calls — it sends a server-INITIATED JSON-RPC request
// (`mcpServer/elicitation/request`, carrying both a method and an id) and
// blocks that tool call until the client answers.
//
// Tether never set agentsessions.StartOptions.JsonRpcRequestHook, so
// agentkit's own fallback answered instead: respondToServerRequest replies
// with a method-not-handled error whenever the hook is nil, deliberately,
// "so the child fails fast instead of hanging". Codex reads that error as a
// refusal and fails the tool call with "user rejected MCP tool call".
//
// The observable result was that EVERY MCP tool call from a Tether-launched
// codex worker failed, in one of two ways depending on the planted
// approval_policy:
//
//   - `never`       — codex refuses before it ever asks: "MCP tool call
//                     requires approval, but approval policy is never".
//   - `on-request`  — codex asks, agentkit's fallback errors, codex reports
//                     "user rejected MCP tool call".
//
// Per-tool `[mcp_servers.<server>.tools.<tool>] approval_mode = "auto"` does
// NOT bypass either path (verified against codex-cli 0.154.0), and would be
// unusable here anyway: the planted server is the mux PROXY, which fronts
// several hundred tools whose names Tether does not know at plant time.
//
// So both halves are required, and neither works alone:
//   1. the planted approval_policy must be a value that ASKS rather than
//      refusing outright (runtime_resolver.go sets "on-request"), and
//   2. something must answer — this file.
//
// Policy: a launched worker gets open MCP access by default. An agent that
// cannot call the tools its own launch planted for it is not sandboxed, it
// is broken — the same reasoning MuxMCPArgs already states about scopes.
// Approval here is a per-call human-in-the-loop gate, and a Tether-launched
// worker has no human in its loop by construction; leaving the gate closed
// does not make it safer, it makes it inert. Real authorization belongs in
// the broker and in the MCP scope set, both of which still apply to every
// call this approves.
//
// Deliberately narrow: only MCP tool-call elicitations are approved. Any
// other server-initiated request keeps agentkit's fail-fast behavior rather
// than being blanket-approved, so a future codex approval kind (an exec
// escalation, a network amendment) surfaces as a visible failure instead of
// being silently granted by a hook that was written before it existed.

import (
	"encoding/json"
	"log"

	"github.com/hollis-labs/agentkit/agentsessions"
)

const (
	// codexElicitationMethod is the server-initiated request codex
	// app-server sends to gate an approval-requiring action.
	codexElicitationMethod = "mcpServer/elicitation/request"

	// codexApprovalKindMCPToolCall is the `_meta.codex_approval_kind`
	// discriminator identifying an elicitation as an MCP tool-call
	// approval. Other kinds are left to agentkit's fallback.
	codexApprovalKindMCPToolCall = "mcp_tool_call"

	// elicitationActionAccept is the MCP elicitation response action
	// granting the request. Codex's vocabulary is accept | decline |
	// cancel.
	elicitationActionAccept = "accept"
)

// codexElicitationParams is the subset of an elicitation request Tether
// reads. Everything else (message, requestedSchema, threadId, turnId) is
// presentation or correlation detail this hook does not need.
type codexElicitationParams struct {
	ServerName string `json:"serverName"`
	Meta       struct {
		ApprovalKind string `json:"codex_approval_kind"`
		ToolName     string `json:"tool_name"`
	} `json:"_meta"`
}

// elicitationAccept is the MCP elicitation response. `content` is required
// by the spec even when the requested schema is empty, which is the shape
// codex asks for on a tool-call approval, so it is always emitted rather
// than omitted when empty.
type elicitationAccept struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content"`
}

// jsonRPCRequestHook returns the handler for server-initiated JSON-RPC
// requests from a jsonrpc-stdio child. Returning (nil, nil) is NOT an
// option for an unhandled method: that marshals to a successful `null`
// result, which codex would read as an approval. Unhandled methods return
// an explicit error so the child fails fast, matching what agentkit does
// when no hook is installed at all.
func jsonRPCRequestHook(sessionID string) func(string, json.RawMessage) (any, *agentsessions.JsonRpcError) {
	return func(method string, params json.RawMessage) (any, *agentsessions.JsonRpcError) {
		if method != codexElicitationMethod {
			return nil, unhandledServerRequest(method)
		}
		var p codexElicitationParams
		if err := json.Unmarshal(params, &p); err != nil {
			// Malformed params for a method we otherwise handle: refuse
			// rather than approve something we could not read.
			log.Printf("app: codex elicitation on session %s: decode params: %v (refusing)", sessionID, err)
			return nil, unhandledServerRequest(method)
		}
		if p.Meta.ApprovalKind != codexApprovalKindMCPToolCall {
			return nil, unhandledServerRequest(method)
		}
		log.Printf("app: codex elicitation on session %s: approving %s tool call on server %q", sessionID, p.Meta.ApprovalKind, p.ServerName)
		return elicitationAccept{Action: elicitationActionAccept, Content: map[string]any{}}, nil
	}
}

// unhandledServerRequest mirrors the error agentkit's own nil-hook fallback
// returns, so installing this hook never turns a previously fail-fast
// request into a hang.
func unhandledServerRequest(method string) *agentsessions.JsonRpcError {
	return &agentsessions.JsonRpcError{
		Code:    -32601,
		Message: "method not handled by tether: " + method,
	}
}
