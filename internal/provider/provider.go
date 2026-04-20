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
)

// RuntimeKind names the execution family a provider runtime belongs to.
// The runtime manager treats both kinds uniformly; downstream consumers
// (API, observability) may branch on this to expose mode-specific affordances.
type RuntimeKind string

const (
	RuntimeKindCLI RuntimeKind = "cli"
	RuntimeKindAPI RuntimeKind = "api"
)

// HealthStatus snapshots a live session's liveness. PID is meaningful only
// for CLI runtimes; API-backed sessions report PID=0 and rely on Alive.
type HealthStatus struct {
	Alive bool
	PID   int
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
type StartOptions struct {
	Workdir    string
	LogPath    string
	BootPrompt string
	BootMode   string
	Fanout     io.Writer
}

// Runtime is the high-level contract a provider adapter satisfies. It names
// itself and its kind, validates a plan via Prepare, and spawns a Session
// when asked to Start.
//
// Prepare is called once per launch before Start, giving the runtime a
// chance to surface configuration errors (missing binaries, invalid API
// keys) while the caller can still abort cleanly without leaving half-
// created workspace artifacts.
type Runtime interface {
	ID() string
	Kind() RuntimeKind
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
