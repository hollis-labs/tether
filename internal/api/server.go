package api

import (
	"context"
	"net/http"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
	"github.com/hollis-labs/tether/internal/store"
)

// AttachmentStore is the narrow storage contract for listing client
// attachment history per session. *store.Store satisfies it directly.
type AttachmentStore interface {
	ListClientAttachments(sessionID string) ([]store.ClientAttachmentRow, error)
}

// Deps bundles everything the api handlers need at construction time.
// The daemon package builds this and calls NewHandler to get an
// http.Handler back. Each field is optional — handlers whose
// dependency is nil return 404 for their routes rather than panicking.
type Deps struct {
	Service     LaunchService
	AI          AIService
	AIAudit     AIAuditStore
	AIUsage     AIUsageStore
	Checkpoints CheckpointStore
	Broker      BrokerService
	Bus         events.Bus
	EventsStore EventsStore
	Catalog     CatalogLoader
	GroupStore  SessionGroupStore
	// Workstreams, when non-nil, enables the /workstreams endpoints and the
	// per-session workstream sub-resource (S1, CW-20260912-0059).
	Workstreams WorkstreamStore
	// SessionRefs, when non-nil, enables /sessions/{id}/refs and the
	// /workstreams/{id}/refs roll-up (S2, CW-20260912-0060).
	SessionRefs SessionRefStore
	// Digests, when non-nil, enables /sessions/{id}/digest,
	// /workstreams/{id}/digest and the ?ref= reverse lookup on the
	// /workstreams collection (S5, CW-20260912-0063).
	Digests      DigestStore
	MessageStore MessageStore
	// ProxyEvents, when non-nil, enables the /proxy/events endpoint for
	// persisting and querying MCP relay tool call events. Populated by the
	// daemon when --proxy mode is active. The TUI polls this to populate the
	// Activity feed without requiring in-process access to the MCP subprocess.
	ProxyEvents ProxyEventStore
	// Attachments, when non-nil, enables GET /sessions/{id}/attachments.
	Attachments AttachmentStore
	// Registry, when non-nil, enables the /registry/... federation
	// directory routes wired in registerRegistryRoutes. Absent the dep,
	// the daemon falls through to its 404 default — matches the
	// Catalog/Broker convention.
	Registry RegistryService

	// RegistryCatalogRoot is the catalog root passed to the bootstrap
	// importer when handling `POST /registry/bootstrap`. Empty disables
	// the endpoint (returns 503), matching the rest of the package's
	// nil-disables-route convention. The daemon wiring fills this in
	// from the *app.Service's CatalogRoot; in-process tests can set it
	// to a temp catalog dir.
	RegistryCatalogRoot string

	// Groups, when non-nil, enables the /groups/... + /mentions
	// v060-05 group-messaging routes (registerGroupRoutes). In
	// production this is the same *registry.Service instance used by
	// Registry — the GroupsService interface is a narrow seam over the
	// group-specific methods. Splitting the field lets tests wire a
	// focused stub for group surfaces without faking the full Registry
	// CRUD vocabulary.
	Groups GroupsService

	// LogsDir, when non-empty, enables GET /logs/daemon serving a bounded
	// tail of LogsDir/muxd.log. Empty disables the endpoint (returns 404).
	LogsDir string

	// DeliveryClaims, when non-nil, enables POST /messages/{id}/claim|ack|nack
	// (T07, messaging vNext) -- durable claim/ack/nack for a caller pulling
	// its own mailbox on its own initiative (a published-local bridge, or
	// any other caller), rather than Tether pushing a wake. *app.Service's
	// underlying *store.Store satisfies it directly. Absent the dep, these
	// three actions respond 404 (matches every other optional dependency).
	DeliveryClaims DeliveryClaimer

	// SessionBootstrap, when non-nil, enables POST /sessions/bootstrap
	// (T08, messaging vNext) -- the provider-neutral local bootstrap/
	// registration helper an external launcher (agent-setup) can invoke
	// at the launch/host boundary. *app.Service's underlying *store.Store
	// satisfies it directly. Absent the dep, the route 404s (matches
	// every other optional dependency).
	SessionBootstrap SessionBootstrapStore

	// DeliveryTrace, when non-nil, enables GET /messages/{id}/trace (T09,
	// messaging vNext) -- the structured delivery trace joining
	// message/delivery/attempt/receipt data with binding history.
	// *app.Service's underlying *store.Store satisfies it directly.
	// Absent the dep, the trace action responds 404 (matches every other
	// optional dependency).
	DeliveryTrace DeliveryTraceStore

	// DeliveryRepair, when non-nil, enables POST /messages/{id}/redrive
	// (T09, messaging vNext) -- authorized retry/redrive of a dead-
	// lettered delivery. Deliberately a SEPARATE field from DeliveryTrace
	// (same underlying capability, same *store.Store satisfies both)
	// so an operator can wire read-only tracing without also granting
	// write/repair capability. Absent the dep, the redrive action
	// responds 404 (matches every other optional dependency).
	DeliveryRepair DeliveryTraceStore

	// Retention, when non-nil, enables GET /messages/retention/candidates
	// and POST /messages/{id}/purge (T09, messaging vNext) -- the
	// explicit, manual-only message-body retention/purge surface.
	// *app.Service's underlying *store.Store satisfies it directly.
	// Absent the dep, both actions respond 404 (matches every other
	// optional dependency).
	Retention RetentionStore
}

// Server carries the dependencies required by handlers. Tests construct
// it directly; production code goes through NewHandler.
type Server struct {
	Service             LaunchService
	AI                  AIService
	AIAudit             AIAuditStore
	AIUsage             AIUsageStore
	Checkpoints         CheckpointStore
	Broker              BrokerService
	Bus                 events.Bus
	EventsStore         EventsStore
	Catalog             CatalogLoader
	GroupStore          SessionGroupStore
	Workstreams         WorkstreamStore
	SessionRefs         SessionRefStore
	Digests             DigestStore
	MessageStore        MessageStore
	ProxyEvents         ProxyEventStore
	Attachments         AttachmentStore
	Registry            RegistryService
	RegistryCatalogRoot string
	Groups              GroupsService
	LogsDir             string
	DeliveryClaims      DeliveryClaimer
	SessionBootstrap    SessionBootstrapStore
	DeliveryTrace       DeliveryTraceStore
	DeliveryRepair      DeliveryTraceStore
	Retention           RetentionStore
}

// NewHandler builds the http.Handler serving every route owned by the
// api package. The daemon package layers /health on top of this.
func NewHandler(deps Deps) http.Handler {
	s := &Server{
		Service:             deps.Service,
		AI:                  deps.AI,
		AIAudit:             deps.AIAudit,
		AIUsage:             deps.AIUsage,
		Checkpoints:         deps.Checkpoints,
		Broker:              deps.Broker,
		Bus:                 deps.Bus,
		EventsStore:         deps.EventsStore,
		Catalog:             deps.Catalog,
		GroupStore:          deps.GroupStore,
		Workstreams:         deps.Workstreams,
		SessionRefs:         deps.SessionRefs,
		Digests:             deps.Digests,
		MessageStore:        deps.MessageStore,
		ProxyEvents:         deps.ProxyEvents,
		Attachments:         deps.Attachments,
		Registry:            deps.Registry,
		RegistryCatalogRoot: deps.RegistryCatalogRoot,
		Groups:              deps.Groups,
		LogsDir:             deps.LogsDir,
		DeliveryClaims:      deps.DeliveryClaims,
		SessionBootstrap:    deps.SessionBootstrap,
		DeliveryTrace:       deps.DeliveryTrace,
		DeliveryRepair:      deps.DeliveryRepair,
		Retention:           deps.Retention,
	}
	mux := http.NewServeMux()
	s.registerSessionRoutes(mux)
	s.registerAIRoutes(mux)
	s.registerCheckpointRoutes(mux)
	s.registerBrokerRoutes(mux)
	s.registerEventRoutes(mux)
	s.registerCatalogRoutes(mux)
	s.registerSessionGroupRoutes(mux)
	s.registerWorkstreamRoutes(mux)
	s.registerMessageRoutes(mux)
	s.registerProxyEventRoutes(mux)
	s.registerRegistryRoutes(mux)
	s.registerGroupRoutes(mux)
	s.registerWhoamiRoutes(mux)
	s.registerSessionBootstrapRoutes(mux)
	s.registerRetentionRoutes(mux)
	s.registerScopedBindingRoutes(mux)
	s.registerLogsRoutes(mux)
	s.registerFSRoutes(mux)
	return mux
}

// AIService is the narrow AI gateway seam exposed over /ai/*.
type AIService interface {
	Chat(ctx context.Context, req llm.Request) (llm.Response, error)
	Embed(ctx context.Context, req llm.Request) (llm.Response, error)
	StreamChat(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error)
	PreviewRoute(req llm.Request) (router.Plan, error)
	ExplainRoute(req llm.Request) (router.Explanation, error)
	ListProviders() []llmservice.ProviderInfo
	ListModels(providerID string) []modelsdev.ModelRef
	ListRoutes() []router.Route
}
