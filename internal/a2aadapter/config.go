package a2aadapter

import "time"

// defaultTaskAwaitTimeout bounds how long a delegated-work Execute call
// waits for a consumer transition before giving up and reporting
// InputRequired (see executor.go). A relatively short default keeps
// fixture tests fast; production callers configure their own via
// AgentBinding.TaskAwaitTimeout.
const defaultTaskAwaitTimeout = 30 * time.Second

// AgentBinding opts one Tether-registered agent into A2A reachability.
// Deliberately plain config (no registry.Profile dependency) so this
// package stays testable without registry/store setup boilerplate — the
// daemon wiring layer is responsible for deriving these fields from the
// registry when it constructs a Config for production use.
type AgentBinding struct {
	// ID names this binding in the URL path (/a2a/agents/{ID}/...) and in
	// the synthetic messaging.KindService address representing the
	// external A2A peer (msg://service/a2a/{ID}). Must be non-empty and
	// URL-path-safe.
	ID string

	// TargetURN is the Tether recipient address (any messaging.Address-
	// parseable URN) inbound A2A messages are relayed to. Required.
	TargetURN string

	// DisplayName and Description populate the advertised a2a.AgentCard.
	DisplayName string
	Description string

	// BaseURL is the externally-reachable base URL this binding is
	// served at (e.g. a fixture httptest.Server's URL in tests, or an
	// operator-configured public URL in front of a reverse proxy in a
	// real deployment). Required — the AgentCard's SupportedInterfaces
	// entry needs an absolute URL, and this package does not guess one
	// from the daemon's own (usually UDS-only) listen address.
	BaseURL string

	// BearerToken, when non-empty, is required on every request via the
	// standard `Authorization: Bearer <token>` header (auth.go). Empty
	// means no bearer-token check for this binding — same-host-only
	// fixture/test usage is expected to leave this empty; anything
	// meant to be reachable beyond localhost should set one.
	BearerToken string

	// TaskMode, when true, means every inbound message on this binding
	// is treated as delegated work: the executor emits a Task and waits
	// for an explicit consumer transition (see executor.go, transition.go).
	// When false (the default), every inbound message stays message-only
	// — no Task is ever created, satisfying T10 acceptance #2's "generic
	// messages are not forced into tasks."
	TaskMode bool

	// TaskAwaitTimeout bounds how long a TaskMode binding's Execute call
	// waits for a consumer transition. Zero uses defaultTaskAwaitTimeout.
	TaskAwaitTimeout time.Duration
}

func (b AgentBinding) awaitTimeout() time.Duration {
	if b.TaskAwaitTimeout > 0 {
		return b.TaskAwaitTimeout
	}
	return defaultTaskAwaitTimeout
}

// Config is the full set of A2A-opted-in bindings for one daemon. An empty
// Config (or a nil *Adapter, see adapter.go) means the A2A surface is
// absent entirely — "the feature stays optional for local messaging"
// (T10 acceptance #3): nothing about plain Tether messaging changes or
// depends on this package being wired in at all.
type Config struct {
	Bindings []AgentBinding
}
