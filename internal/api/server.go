package api

import (
	"net/http"

	"github.com/hollis-labs/tether/internal/events"
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
}

// Server carries the dependencies required by handlers. Tests construct
// it directly; production code goes through NewHandler.
type Server struct {
	Service      LaunchService
	Checkpoints  CheckpointStore
	Broker       BrokerService
	Bus          events.Bus
	EventsStore  EventsStore
	Catalog      CatalogLoader
	GroupStore   SessionGroupStore
	MessageStore MessageStore
	ProxyEvents  ProxyEventStore
	Attachments  AttachmentStore
}

// NewHandler builds the http.Handler serving every route owned by the
// api package. The daemon package layers /health on top of this.
func NewHandler(deps Deps) http.Handler {
	s := &Server{
		Service:      deps.Service,
		Checkpoints:  deps.Checkpoints,
		Broker:       deps.Broker,
		Bus:          deps.Bus,
		EventsStore:  deps.EventsStore,
		Catalog:      deps.Catalog,
		GroupStore:   deps.GroupStore,
		MessageStore: deps.MessageStore,
		ProxyEvents:  deps.ProxyEvents,
		Attachments:  deps.Attachments,
	}
	mux := http.NewServeMux()
	s.registerSessionRoutes(mux)
	s.registerCheckpointRoutes(mux)
	s.registerBrokerRoutes(mux)
	s.registerEventRoutes(mux)
	s.registerCatalogRoutes(mux)
	s.registerSessionGroupRoutes(mux)
	s.registerMessageRoutes(mux)
	s.registerProxyEventRoutes(mux)
	return mux
}
