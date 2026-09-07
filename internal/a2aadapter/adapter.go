package a2aadapter

// adapter.go — wires every configured AgentBinding into one http.Handler
// tree. See doc.go for the overall design; config.go for AgentBinding;
// executor.go/coordinator.go for the relay + task-transition machinery.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	messaging "github.com/hollis-labs/go-messaging"
)

// Adapter serves every AgentBinding in a Config. Mux() returns the full
// http.Handler tree; mount it under a namespaced prefix (e.g. "/a2a/")
// in the daemon via http.StripPrefix. A nil *Adapter (or simply never
// constructing one) means the A2A surface is absent entirely -- nothing
// about Tether's own messaging depends on this package (T10 acceptance
// #3: "the feature stays optional for local messaging").
type Adapter struct {
	coordinator *taskCoordinator
	mux         *http.ServeMux
}

// NewAdapter builds an Adapter for cfg. sender is the canonical Tether
// Send path (*store.Store's InboxStore, via MessagingStore(), satisfies
// it directly). Construction fails closed on any misconfigured binding
// (empty/duplicate ID, empty TargetURN/BaseURL, or an unparseable
// TargetURN) rather than silently skipping it.
func NewAdapter(cfg Config, sender MessageSender) (*Adapter, error) {
	a := &Adapter{coordinator: newTaskCoordinator(), mux: http.NewServeMux()}
	seen := make(map[string]struct{}, len(cfg.Bindings))

	for _, b := range cfg.Bindings {
		if b.ID == "" {
			return nil, fmt.Errorf("a2aadapter: binding has empty ID")
		}
		if strings.ContainsAny(b.ID, "/ \t\n") {
			return nil, fmt.Errorf("a2aadapter: binding %q: ID must not contain '/' or whitespace", b.ID)
		}
		if _, dup := seen[b.ID]; dup {
			return nil, fmt.Errorf("a2aadapter: duplicate binding ID %q", b.ID)
		}
		seen[b.ID] = struct{}{}
		if b.TargetURN == "" {
			return nil, fmt.Errorf("a2aadapter: binding %q: TargetURN is required", b.ID)
		}
		target, err := messaging.ParseURN(b.TargetURN)
		if err != nil {
			return nil, fmt.Errorf("a2aadapter: binding %q: invalid TargetURN: %w", b.ID, err)
		}
		if b.BaseURL == "" {
			return nil, fmt.Errorf("a2aadapter: binding %q: BaseURL is required", b.ID)
		}

		executor := &TetherExecutor{binding: b, target: target, sender: sender, coordinator: a.coordinator}

		opts := []a2asrv.RequestHandlerOption{
			// Streaming/PushNotifications always false: this binding
			// never advertises a capability it doesn't implement, and
			// the SDK itself enforces the resulting ErrUnsupportedOperation
			// / ErrPushNotificationNotSupported responses (T10 acceptance
			// #3) -- this package adds no signaling logic of its own.
			a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: false, PushNotifications: false}),
		}
		if b.BearerToken != "" {
			opts = append(opts, a2asrv.WithCallInterceptors(&bearerTokenInterceptor{token: b.BearerToken}))
		}
		reqHandler := a2asrv.NewHandler(executor, opts...)

		rpcPath := "/agents/" + b.ID + "/rpc"
		rpcURL := strings.TrimSuffix(b.BaseURL, "/") + rpcPath
		card := buildAgentCard(b, rpcURL)

		a.mux.Handle("/agents/"+b.ID+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
		a.mux.Handle(rpcPath, a2asrv.NewJSONRPCHandler(reqHandler))

		taskPrefix := "/agents/" + b.ID + "/tasks/"
		a.mux.HandleFunc(taskPrefix, a.transitionRouterFor(taskPrefix, b.ID, b.BearerToken))
	}

	return a, nil
}

// transitionRouterFor returns a handler parsing "{taskID}/transition" off
// prefix, matching this codebase's established prefix-then-parse routing
// idiom (see internal/api/registry.go's doc comment on the same choice).
// bindingID and bearerToken are this specific binding's own values,
// captured once per binding here rather than re-derived per request.
func (a *Adapter) transitionRouterFor(prefix, bindingID, bearerToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		taskID, action, ok := strings.Cut(rest, "/")
		if !ok || action != "transition" || taskID == "" {
			http.NotFound(w, r)
			return
		}
		a.handleTransition(w, r, bindingID, taskID, bearerToken)
	}
}

// Mux returns the full A2A HTTP surface for every configured binding.
func (a *Adapter) Mux() http.Handler {
	return a.mux
}
