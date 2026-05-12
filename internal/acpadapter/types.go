// Package acpadapter exposes the agent-mux service layer as an Agent Client
// Protocol (ACP) server over stdio. ACP is a JSON-RPC 2.0 protocol used by
// editors (Zed, JetBrains, Avante.nvim, CodeCompanion.nvim) to drive AI
// agent sessions.
//
// Spec: https://agentclientprotocol.com
//
// This package is designed for future extraction to a portfolio go-acp
// library (see Vanta `followup_portfolio_go_acp_extraction`). Therefore:
//
//   - No direct imports of mux-internal types. Mux integrates via the
//     Service interface declared in service.go.
//   - Stdio framing, bidirectional JSON-RPC dispatcher, request-response
//     correlator, and auth middleware live in their own files with no
//     mux-specific cruft.
//   - Mux-specific glue (boot profile selection, MCP server resolution,
//     sandbox profile lookup) lives in cmd/mux/acp.go and internal/app/,
//     NOT inside this package.
//
// MVP scope per the v005-09 method-mapping doc:
//
//	Inbound (client → agent): initialize, authenticate, session/new,
//	session/prompt, session/cancel (notif), session/close, session/resume.
//
//	Outbound (agent → client): session/update (notif) — agent_message_chunk
//	variant only. Bidirectional framing supports outbound requests for
//	future fs/terminal/permission proxying, no MVP code path triggers them.
//
//	Declined (return method-not-found): session/load, session/list,
//	session/set_mode, session/set_config_option, fs/*, terminal/*,
//	session/request_permission.
package acpadapter

import "encoding/json"

// ProtocolVersion is the ACP protocol major version this server speaks.
// The spec uses single-integer MAJOR versions; client sends its latest,
// agent responds with its own latest if mismatched.
const ProtocolVersion = 1

// ─── initialize / authenticate ────────────────────────────────────────────────

// InitializeParams is the params object on the inbound `initialize` request.
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         Implementation     `json:"clientInfo"`
}

// InitializeResult is the agent's response to `initialize`.
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         Implementation    `json:"agentInfo"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

// Implementation identifies a peer (client or agent) by name + version.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ClientCapabilities describes what the editor supports. Mux accepts
// whatever the editor advertises but doesn't call back into the editor
// for fs/terminal in MVP, so the values don't gate any agent-side work.
type ClientCapabilities struct {
	FS       FileSystemCapabilities `json:"fs,omitempty"`
	Terminal bool                   `json:"terminal,omitempty"`
}

// FileSystemCapabilities advertises the editor's fs/* support.
type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

// AgentCapabilities describes what Mux as an ACP agent supports.
// Per v005-09 §1 lock: loadSession=false (no history-replay), resume=true
// (no-replay reconnect for multi-client). All prompt + MCP capabilities
// are off MVP — text-only prompts, MCP via boot-profile (not editor-supplied).
type AgentCapabilities struct {
	LoadSession         bool                     `json:"loadSession"`
	PromptCapabilities  PromptCapabilities       `json:"promptCapabilities"`
	McpCapabilities     McpCapabilities          `json:"mcpCapabilities"`
	SessionCapabilities AgentSessionCapabilities `json:"sessionCapabilities,omitempty"`
}

// PromptCapabilities gates non-text content variants in `session/prompt` params.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// McpCapabilities advertises agent-supported MCP transports for
// editor-supplied `mcpServers` in `session/new`. Mux declines MVP — see §2 lock.
type McpCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// AgentSessionCapabilities advertises optional session lifecycle methods.
type AgentSessionCapabilities struct {
	List   bool `json:"list,omitempty"`
	Close  bool `json:"close,omitempty"`
	Resume bool `json:"resume,omitempty"`
}

// AuthMethod is one entry in the agent's advertised authentication menu.
// Mux mirrors `mux mcp` token auth: a single method with id "token" that
// expects the bearer token in `authenticate` params.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AuthenticateParams is the params object on `authenticate`.
type AuthenticateParams struct {
	MethodID string          `json:"methodId"`
	Body     json.RawMessage `json:"body,omitempty"`
}

// AuthenticateTokenBody is the expected shape inside AuthenticateParams.Body
// when MethodID == "token". Kept as a separate type so future auth methods
// can introduce their own body shapes without breaking the wire envelope.
type AuthenticateTokenBody struct {
	Token string `json:"token"`
}

// ─── session lifecycle ────────────────────────────────────────────────────────

// SessionID is the unique identifier for an ACP conversation session.
// Mux returns its own session UUIDs verbatim — no separate ACP session id
// space — so editors can cross-reference with `mux sessions get <id>`.
type SessionID string

// NewSessionParams is the params object on `session/new`.
type NewSessionParams struct {
	CWD        string            `json:"cwd"`
	McpServers []McpServerConfig `json:"mcpServers,omitempty"`
}

// NewSessionResult is the agent's response to `session/new`.
type NewSessionResult struct {
	SessionID SessionID `json:"sessionId"`
}

// ResumeSessionParams is the params object on `session/resume`.
type ResumeSessionParams struct {
	SessionID  SessionID         `json:"sessionId"`
	CWD        string            `json:"cwd,omitempty"`
	McpServers []McpServerConfig `json:"mcpServers,omitempty"`
}

// ResumeSessionResult is the agent's response to `session/resume`.
// Empty object on success; future config state may be added.
type ResumeSessionResult struct{}

// CloseSessionParams is the params object on `session/close`.
type CloseSessionParams struct {
	SessionID SessionID `json:"sessionId"`
}

// CloseSessionResult is the agent's response to `session/close`. Empty.
type CloseSessionResult struct{}

// McpServerConfig is one entry in the editor-supplied MCP server list.
// Per v005-09 §2 lock: Mux ignores this MVP and uses boot-profile MCPs
// only. Type stays defined so the wire payload can be parsed and logged.
type McpServerConfig struct {
	// Discriminator field per the union types in the schema (stdio | http | sse).
	// We don't act on the contents MVP; capture as raw JSON to forward
	// to the warning log without losing fidelity.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON captures the raw bytes so we can log what the editor sent.
func (m *McpServerConfig) UnmarshalJSON(b []byte) error {
	m.Raw = append(m.Raw[:0], b...)
	return nil
}

// ─── prompt turn ──────────────────────────────────────────────────────────────

// PromptParams is the params object on `session/prompt`.
type PromptParams struct {
	SessionID SessionID      `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResult is the agent's response to `session/prompt`. Stop reason
// per the spec; see StopReason constants.
type PromptResult struct {
	StopReason StopReason `json:"stopReason"`
}

// StopReason enumerates how a prompt turn ended.
type StopReason string

const (
	StopReasonEndTurn         StopReason = "end_turn"
	StopReasonMaxTokens       StopReason = "max_tokens"
	StopReasonMaxTurnRequests StopReason = "max_turn_requests"
	StopReasonRefusal         StopReason = "refusal"
	StopReasonCancelled       StopReason = "cancelled" //nolint:misspell // ACP spec wire value uses British spelling
)

// CancelParams is the params object on the `session/cancel` notification.
type CancelParams struct {
	SessionID SessionID `json:"sessionId"`
}

// ─── content blocks ───────────────────────────────────────────────────────────

// ContentBlock is the union of prompt content variants. Per v005-09 the
// agent declares only text capability, so editors should only send
// {type:"text"} or {type:"resource_link"} (resource_link has no
// promptCapability gate per the spec). Image/audio/embeddedContext blocks
// are rejected by the prompt handler.
type ContentBlock struct {
	Type string `json:"type"`

	// type=text
	Text string `json:"text,omitempty"`

	// type=image | audio
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`

	// type=resource_link
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`

	// type=resource (embedded). Not supported MVP — declared here for
	// completeness; promptCapabilities.embeddedContext=false rejects it.
	Resource json.RawMessage `json:"resource,omitempty"`

	Annotations json.RawMessage `json:"annotations,omitempty"`
}

const (
	ContentTypeText         = "text"
	ContentTypeImage        = "image"
	ContentTypeAudio        = "audio"
	ContentTypeResource     = "resource"
	ContentTypeResourceLink = "resource_link"
)

// ─── session/update notifications ─────────────────────────────────────────────

// SessionUpdateNotification is the wire shape for the agent → client
// `session/update` notification. The discriminator is `update.sessionUpdate`.
type SessionUpdateNotification struct {
	SessionID SessionID     `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// SessionUpdate is the discriminated union of update variants. MVP emits
// only the agent_message_chunk variant; other variants are declared here
// for completeness and future use.
type SessionUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`

	// sessionUpdate=agent_message_chunk
	Content *ContentBlock `json:"content,omitempty"`

	// sessionUpdate=tool_call | tool_call_update — deferred per v005-09 §5
	// (we don't translate underlying CLI agent tool calls to ACP MVP).
	// Fields kept private to this package; serialized via raw maps when
	// needed in future iterations.
}

const (
	SessionUpdateAgentMessageChunk = "agent_message_chunk"
	SessionUpdateToolCall          = "tool_call"
	SessionUpdateToolCallUpdate    = "tool_call_update"
	SessionUpdatePlan              = "plan"
)
