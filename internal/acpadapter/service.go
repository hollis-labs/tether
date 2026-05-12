package acpadapter

import "context"

// Service is the contract this package needs from a host (Mux). It is
// the extraction seam: when this package promotes to a portfolio go-acp
// library, this interface stays unchanged and Mux supplies the
// implementation. No host-internal types leak into the package.
//
// The four lifecycle methods mirror the daemon-routed shape used by
// internal/mcpadapter (Phase 2 of v005-09): create+launch is one call
// since editors never want a separate "created" state, and Stop +
// Resume are direct handles. SendTurn is long-lived — it returns when
// the underlying agent's turn completes, with the wire-level stop
// reason inferred from the host's signal.
//
// Streaming output is delivered out-of-band via the Updates channel
// returned from SendTurn. The dispatcher fans these into outbound
// `session/update` notifications and closes the channel on turn
// completion.
type Service interface {
	// LaunchSession creates and starts a session for the given launch
	// profile + workspace, returning the new session ID. The agent ID
	// for the launch is decided by the host (typically via a
	// command-line flag passed to `mux acp`). The cwd is the editor's
	// current working directory.
	//
	// Returns ErrUnsupported if the host can't satisfy the launch
	// (no agent configured, etc.); the dispatcher maps this to a
	// JSON-RPC error response with code = ErrCodeInternal.
	LaunchSession(ctx context.Context, params LaunchInput) (SessionID, error)

	// SendTurn delivers a user message to the named session and streams
	// agent output via the returned Updates channel until the turn
	// completes (channel close + final StopReason). The implementation
	// is responsible for honoring ctx cancellation.
	SendTurn(ctx context.Context, sessionID SessionID, prompt []ContentBlock) (<-chan TurnUpdate, error)

	// CancelTurn signals an in-flight SendTurn to wrap up early. The
	// SendTurn channel should subsequently emit a terminal TurnUpdate
	// with StopReasonCancelled (the ACP spec wire value uses British
	// spelling — see types.go for the constant).
	CancelTurn(ctx context.Context, sessionID SessionID) error

	// CloseSession terminates the session and frees host-side resources.
	CloseSession(ctx context.Context, sessionID SessionID) error

	// ResumeSession reattaches to an existing session by ID. Used by
	// multi-client attach: a second editor connects via session/resume
	// with a known ID and starts receiving streaming updates from the
	// session's existing event stream. Returns ErrSessionNotFound when
	// the ID is unknown.
	ResumeSession(ctx context.Context, sessionID SessionID, cwd string) error
}

// LaunchInput carries the host-resolved launch parameters for a new
// session. Fields surface what the editor sent on session/new plus any
// host-side defaults the dispatcher resolved.
type LaunchInput struct {
	// CWD is the editor's current working directory, passed verbatim
	// from `session/new` params.
	CWD string

	// EditorMcpServers is the raw editor-supplied MCP server list,
	// retained for logging/observability. v005-09 ignores it (uses
	// boot-profile MCPs instead) per the §2 lock.
	EditorMcpServers []McpServerConfig
}

// TurnUpdate is one streaming event during a SendTurn. The dispatcher
// translates these into ACP `session/update` notifications.
type TurnUpdate struct {
	// Kind selects which fields are populated. Use TurnUpdateKind*
	// constants.
	Kind TurnUpdateKind

	// Kind=text: the streaming text delta from the underlying agent.
	Text string

	// Kind=done: the terminal update for the turn. Channel closes
	// after this is delivered; no further updates follow.
	StopReason StopReason
}

// TurnUpdateKind discriminates TurnUpdate variants.
type TurnUpdateKind string

const (
	// TurnUpdateKindText carries an agent_message_chunk text delta.
	TurnUpdateKindText TurnUpdateKind = "text"

	// TurnUpdateKindDone carries the terminal stop-reason update.
	TurnUpdateKindDone TurnUpdateKind = "done"
)

// ─── sentinel errors ──────────────────────────────────────────────────────────

// ErrSessionNotFound indicates the requested SessionID is not known.
// Mapped to JSON-RPC error -32602 (invalid params) by the dispatcher
// since session IDs come from method params.
var ErrSessionNotFound = serviceError("acpadapter: session not found")

// ErrUnsupported indicates the host cannot satisfy the request. Mapped
// to JSON-RPC error -32603 (internal error) by the dispatcher.
var ErrUnsupported = serviceError("acpadapter: operation not supported by host")

// serviceError is a small named type so callers can identify
// package-originated sentinels via errors.Is without importing the
// implementation detail. Mirrors stdlib idioms.
type serviceError string

func (e serviceError) Error() string { return string(e) }
