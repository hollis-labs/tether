// Package provider defines the high-level runtime contract that adapters
// satisfy. A provider can be CLI-backed (PTY, process-spawned) or
// API-backed (streaming SDK); both shapes produce a Session whose lifecycle
// the runtime manager can drive uniformly.
package provider

import (
	"context"
	"errors"
	"io"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/hollis-labs/go-sandbox/sandbox"
)

// RuntimeKind names the execution family a provider runtime belongs to.
// The runtime manager treats both kinds uniformly; downstream consumers
// (API, observability) may branch on this to expose mode-specific affordances.
type RuntimeKind string

const (
	RuntimeKindCLI RuntimeKind = "cli"
	RuntimeKindAPI RuntimeKind = "api"
)

// LiveState describes the fine-grained observable state of a running session
// as reported by the provider adapter. It disambiguates sub-states within the
// Mux "running" lifecycle state so consumers (Clockwork, Nanite) can decide
// whether to send another turn without polling for ErrTurnInFlight.
type LiveState int

const (
	// LiveStateIdle: session is alive and waiting for input. No turn in flight.
	LiveStateIdle LiveState = iota
	// LiveStateProcessing: a turn or subprocess is currently running.
	LiveStateProcessing
	// LiveStateStopped: Stop has been called; Wait will return soon.
	LiveStateStopped
)

// String returns the JSON/API string representation of a LiveState.
func (s LiveState) String() string {
	switch s {
	case LiveStateIdle:
		return "idle"
	case LiveStateProcessing:
		return "processing"
	case LiveStateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Capabilities declares what a Runtime's Sessions support beyond the
// baseline Runtime + Session contract. All fields default to false.
// Adapters set fields to true to declare capabilities they implement.
// Callers must check Caps() before using the corresponding affordance —
// attempting a capability against an adapter that declares false for it
// is undefined behavior (the adapter may no-op, return an error, or panic).
type Capabilities struct {
	// PTY: Session.SendInput writes to a live PTY master. Resize is meaningful.
	// False for all turn-based adapters (opencode, claudestream, goprovider, stub).
	PTY bool

	// Resize: Session.Resize has an observable effect on the terminal dimensions.
	// Requires PTY=true to be meaningful; non-PTY adapters no-op Resize per ADR 0014.
	Resize bool

	// ProviderSessionID: the adapter observes and stores a provider-side session ID
	// (e.g. claude --resume ID, opencode sessionID) for cross-session continuity.
	// When true, the Session implements SessionIDer and ProviderSessionID() returns
	// the stored ID (empty string if no turn has run yet).
	ProviderSessionID bool

	// CheckpointResume: Session.CheckpointHints returns a non-trivial hint (_, true).
	// Consumers may use the hint for cross-session continuity beyond ProviderSessionID.
	CheckpointResume bool

	// BinaryRequired: Prepare will return an error if the provider binary is absent
	// from PATH or the configured path. False only for in-process providers (api-stub).
	BinaryRequired bool
}

// SessionIDer is an optional interface implemented by Session when the adapter
// tracks a provider-side session ID (Caps().ProviderSessionID == true). Callers
// must type-assert to this interface; it is not part of the core Session contract.
type SessionIDer interface {
	ProviderSessionID() string
}

// HealthStatus snapshots a live session's liveness.
// PID is meaningful only for PTY runtimes; API-backed and turn-based sessions
// report PID=0 or the PID of the current turn's subprocess.
// State and TurnID provide fine-grained sub-state within the Mux "running"
// lifecycle state (see LiveState). Both are always set by Health(); the zero
// value (LiveStateIdle, "") is safe for adapters that don't distinguish idle
// from processing.
type HealthStatus struct {
	Alive  bool
	PID    int
	State  LiveState // fine-grained within-running state (added in ADR 0025)
	TurnID string    // opaque turn identifier; set by turn-based adapters during a live turn
}

// CheckpointHint is an opaque, provider-defined blob carrying hints for
// checkpoint placement. The shape is deliberately unpinned in v0.0.2 and
// will be concretised in v0.0.3 alongside Sprint v003-04 — implementations
// should return (zero, false) until the schema is fixed.
type CheckpointHint struct{}

// StartOptions bundles the runtime-agnostic state a Runtime needs to spawn
// a Session. Fanout, if non-nil, receives a copy of the session's output
// stream so the manager can fan it out to attach subscribers — CLI runtimes
// tee the PTY bytes into it; API runtimes write streamed assistant tokens.
//
// ClaudeSessionIDPreset and OnClaudeSessionID are additive hooks for the
// claudestream adapter's `--resume <id>` continuity (T-v004-s02-05). Both
// are optional; adapters that don't understand them (PTY claudecode,
// stub API) ignore them silently.
type StartOptions struct {
	Workdir    string
	LogPath    string
	BootPrompt string
	BootMode   string
	Fanout     io.Writer

	// ClaudeSessionIDPreset, when non-empty, is the claude CLI
	// session_id the adapter should pass as `--resume` on the very
	// first turn. Sourced by the caller from the durable store
	// (logical_agents.claude_session_id) so the conversation continues
	// across daemon restarts.
	ClaudeSessionIDPreset string

	// OnClaudeSessionID, when non-nil, is invoked by the claudestream
	// adapter with the session_id observed on the first `system/init`
	// event of a freshly-spawned turn. Callers typically persist it
	// via *store.Store.SetClaudeSessionID. Called on the adapter's
	// read goroutine — callers must not block inside it.
	OnClaudeSessionID func(claudeSessionID string)

	// Sandbox, when non-nil, is the resolved sandbox profile the provider
	// must enforce before starting the session process. Nil means no
	// enforcement (DefaultSandbox was empty). The provider calls
	// sandbox.Apply to wrap the exec.Cmd before starting it. Per ADR 0013,
	// a non-nil profile on an unsupported platform is a hard launch failure.
	Sandbox *sandbox.Profile
}

// Runtime is the high-level contract a provider adapter satisfies. It names
// itself and its kind, validates a plan via Prepare, and spawns a Session
// when asked to Start.
//
// Caps returns the static capability declaration for Sessions produced by
// this Runtime. The returned Capabilities struct is immutable for the
// lifetime of the Runtime and safe to call from multiple goroutines.
//
// Prepare is called once per launch before Start, giving the runtime a
// chance to surface configuration errors (missing binaries, invalid API
// keys) while the caller can still abort cleanly without leaving half-
// created workspace artifacts.
type Runtime interface {
	ID() string
	Kind() RuntimeKind
	Caps() Capabilities // added in ADR 0025
	Prepare(ctx context.Context, plan *launch.Plan) error
	Start(ctx context.Context, plan *launch.Plan, opts StartOptions) (Session, error)
}

// Session is a running session's control surface. The runtime manager holds
// one Session per registered entry and drives it through its lifecycle.
//
// Wait blocks until the session terminates and returns its exit code. Stop
// requests termination; the watch goroutine will observe the resulting Wait
// return. SendInput pushes input bytes (CLI: PTY master; API: conversation
// append). Health reports current liveness. CheckpointHints optionally
// advises the checkpointer.
//
// SendInput is not required to be safe for concurrent callers; the runtime
// manager serializes it behind a per-session lock.
type Session interface {
	Wait() (int, error)
	Stop(ctx context.Context) error
	SendInput(ctx context.Context, data []byte) error
	// Resize updates the session's terminal winsize so child TUI apps
	// redraw correctly. For CLI/PTY sessions this translates to a
	// TIOCSWINSZ on the PTY master. For API sessions with no terminal
	// concept, implementations should no-op and return nil.
	// Additive to the v0.0.2 contract (ADR 0006, ADR 0014).
	Resize(ctx context.Context, rows, cols uint16) error
	Health() HealthStatus
	CheckpointHints() (CheckpointHint, bool)
}

// ErrNoInputChannel is returned by SendInput when a session does not expose
// a writable input channel — typically a session that has already terminated,
// or a runtime kind that does not accept input mid-flight.
var ErrNoInputChannel = errors.New("session has no input channel")

// Registry maps provider IDs to Runtime implementations. It is not safe for
// concurrent Register/Get; registration happens during composition, lookups
// happen during launch.
type Registry struct {
	byID map[string]Runtime
}

func NewRegistry() *Registry {
	return &Registry{byID: map[string]Runtime{}}
}

func (r *Registry) Register(rt Runtime) { r.byID[rt.ID()] = rt }

func (r *Registry) Get(id string) (Runtime, bool) {
	rt, ok := r.byID[id]
	return rt, ok
}
