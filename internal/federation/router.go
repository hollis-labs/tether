package federation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	messaging "github.com/hollis-labs/go-messaging"
)

// ErrNoRoute is returned by a strict-mode Router for a message whose
// recipient authority is neither the local authority nor a registered
// peer. A non-strict Router never returns it — it falls through to the
// local store instead.
var ErrNoRoute = errors.New("federation: no route for authority")

// Router is a messaging.Store decorator that routes each operation by the
// Authority segment of the recipient URN — the federation seam described
// in docs/messaging-federation.md.
//
// It holds one local store plus a registry of foreign-authority peer
// stores. Routing is keyed on the recipient authority, because delivery is
// always to the daemon that owns the recipient:
//
//   - Send      → env.To.Authority
//   - Inbox     → to.Authority
//   - Subscribe → to.Authority
//   - Consume   → recipient.Authority
//
// Get, Thread, and Cancel take an opaque envelope or thread id rather than
// an Address, so they carry no authority to route on; the Router serves
// them from the local store. Resolving a foreign envelope id requires the
// cross-host transport specified by program task M2 and is out of scope
// here — a caller holding a peer store can invoke those operations on it
// directly.
//
// A standalone install registers no peers, so every authority resolves to
// the local store and the Router behaves exactly as the bare local store:
// federation is purely additive. A Router is itself a messaging.Store, so
// it composes with messaging.NewDispatcher for federated request/reply.
//
// Loop/echo safety (T07, messaging vNext): the Router makes exactly ONE
// routing decision per call and never recurses. storeFor resolves the
// destination store once; that store's own Send/Inbox/etc. either persists
// locally (messagingStore) or issues one HTTP round trip (httpPeerStore) --
// neither path ever calls back into a Router.Send for the same envelope.
// A receiving peer's own daemon just stores the envelope in its local
// database; it does not automatically re-forward it anywhere. So an A<->B
// mutual peer configuration (each treating the other as its one peer) has
// no path that could ping-pong the same envelope indefinitely -- each
// Send is a single, terminal hop, not a chain. What one-hop routing does
// NOT prevent is an operator-authored addressing bug (e.g., a workflow
// that itself re-sends every inbound message back to its sender's
// authority) -- that is an application-level loop, outside the Router's
// authority-routing responsibility, exactly as an email server doesn't
// stop a mail rule that forwards a message back to its own sender.
//
// All methods are safe for concurrent use; the peer registry may be
// mutated while operations are in flight.
type Router struct {
	local          messaging.Store
	localAuthority string
	strict         bool

	mu     sync.RWMutex
	routes map[string]messaging.Store
}

// Router satisfies the go-messaging Store contract.
var _ messaging.Store = (*Router)(nil)

// RouterOption configures a Router at construction.
type RouterOption func(*Router)

// WithStrictRouting makes the Router return ErrNoRoute for an authority
// that is neither local nor a registered peer, rather than falling
// through to the local store. It has no effect when the local authority
// is empty — the Router then cannot tell an unknown authority from the
// local domain.
func WithStrictRouting() RouterOption {
	return func(r *Router) { r.strict = true }
}

// NewRouter constructs a Router around local, the store serving
// localAuthority. Peers are added afterwards with Register; see
// BuildRouter for the config-driven path. local must be non-nil.
func NewRouter(local messaging.Store, localAuthority string, opts ...RouterOption) *Router {
	r := &Router{
		local:          local,
		localAuthority: localAuthority,
		routes:         make(map[string]messaging.Store),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// LocalAuthority returns the authority this Router treats as local.
func (r *Router) LocalAuthority() string { return r.localAuthority }

// Register adds (or replaces) a peer route: messages addressed to
// authority are dispatched to remote. It rejects an empty authority, a
// nil store, and the Router's own local authority.
func (r *Router) Register(authority string, remote messaging.Store) error {
	if authority == "" {
		return fmt.Errorf("federation: Register requires a non-empty authority")
	}
	if remote == nil {
		return fmt.Errorf("federation: Register requires a non-nil store for authority %q", authority)
	}
	if r.localAuthority != "" && authority == r.localAuthority {
		return fmt.Errorf("federation: cannot register a peer for the local authority %q", authority)
	}
	r.mu.Lock()
	r.routes[authority] = remote
	r.mu.Unlock()
	return nil
}

// Unregister removes the peer route for authority, if any. Subsequent
// operations for that authority fall through to the local store (or, in
// strict mode, return ErrNoRoute).
func (r *Router) Unregister(authority string) {
	r.mu.Lock()
	delete(r.routes, authority)
	r.mu.Unlock()
}

// Authorities returns the registered peer authorities, sorted. The local
// authority is never included.
func (r *Router) Authorities() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.routes))
	for a := range r.routes {
		out = append(out, a)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// IsLocal reports whether authority is served by the local store — that
// is, it has no registered peer route.
func (r *Router) IsLocal(authority string) bool {
	r.mu.RLock()
	_, foreign := r.routes[authority]
	r.mu.RUnlock()
	return !foreign
}

// storeFor resolves the store serving authority. A registered peer always
// wins; otherwise the local store handles it, unless strict routing is on
// and the authority is neither local nor routed.
func (r *Router) storeFor(authority string) (messaging.Store, error) {
	r.mu.RLock()
	remote, foreign := r.routes[authority]
	r.mu.RUnlock()
	if foreign {
		return remote, nil
	}
	if r.strict && r.localAuthority != "" && authority != r.localAuthority {
		return nil, fmt.Errorf("%w: %q", ErrNoRoute, authority)
	}
	return r.local, nil
}

// Send routes the envelope to the store owning env.To.Authority.
func (r *Router) Send(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	s, err := r.storeFor(env.To.Authority)
	if err != nil {
		return messaging.Envelope{}, err
	}
	return s.Send(ctx, env)
}

// Inbox routes to the store owning to.Authority.
func (r *Router) Inbox(ctx context.Context, to messaging.Address, f messaging.Filter) ([]messaging.Envelope, error) {
	s, err := r.storeFor(to.Authority)
	if err != nil {
		return nil, err
	}
	return s.Inbox(ctx, to, f)
}

// Subscribe routes to the store owning to.Authority.
func (r *Router) Subscribe(ctx context.Context, to messaging.Address, f messaging.Filter) (<-chan messaging.Envelope, error) {
	s, err := r.storeFor(to.Authority)
	if err != nil {
		return nil, err
	}
	return s.Subscribe(ctx, to, f)
}

// Consume routes to the store owning recipient.Authority.
func (r *Router) Consume(ctx context.Context, id string, recipient messaging.Address) error {
	s, err := r.storeFor(recipient.Authority)
	if err != nil {
		return err
	}
	return s.Consume(ctx, id, recipient)
}

// Get retrieves an envelope by id from the local store — ids carry no
// authority to route on. See the Router doc.
func (r *Router) Get(ctx context.Context, id string) (messaging.Envelope, error) {
	return r.local.Get(ctx, id)
}

// Thread returns a thread from the local store — thread ids carry no
// authority to route on. See the Router doc.
func (r *Router) Thread(ctx context.Context, threadID string, f messaging.Filter) ([]messaging.Envelope, error) {
	return r.local.Thread(ctx, threadID, f)
}

// Cancel marks an envelope dead in the local store — ids carry no
// authority to route on. See the Router doc.
func (r *Router) Cancel(ctx context.Context, id string) error {
	return r.local.Cancel(ctx, id)
}
