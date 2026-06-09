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
	Service      LaunchService
	AI           AIService
	AIAudit      AIAuditStore
	AIUsage      AIUsageStore
	Checkpoints  CheckpointStore
	Broker       BrokerService
	Bus          events.Bus
	EventsStore  EventsStore
	Catalog      CatalogLoader
	GroupStore   SessionGroupStore
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
	MessageStore        MessageStore
	ProxyEvents         ProxyEventStore
	Attachments         AttachmentStore
	Registry            RegistryService
	RegistryCatalogRoot string
	Groups              GroupsService
	LogsDir             string
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
		MessageStore:        deps.MessageStore,
		ProxyEvents:         deps.ProxyEvents,
		Attachments:         deps.Attachments,
		Registry:            deps.Registry,
		RegistryCatalogRoot: deps.RegistryCatalogRoot,
		Groups:              deps.Groups,
		LogsDir:             deps.LogsDir,
	}
	mux := http.NewServeMux()
	s.registerSessionRoutes(mux)
	s.registerAIRoutes(mux)
	s.registerCheckpointRoutes(mux)
	s.registerBrokerRoutes(mux)
	s.registerEventRoutes(mux)
	s.registerCatalogRoutes(mux)
	s.registerSessionGroupRoutes(mux)
	s.registerMessageRoutes(mux)
	s.registerProxyEventRoutes(mux)
	s.registerRegistryRoutes(mux)
	s.registerGroupRoutes(mux)
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
