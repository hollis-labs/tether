package api

import (
	"net/http"
)

// Deps bundles everything the api handlers need at construction time.
// The daemon package builds this and calls NewHandler to get an
// http.Handler back. Each field is optional — handlers whose
// dependency is nil return 404 for their routes rather than panicking.
type Deps struct {
	Service     LaunchService
	Checkpoints CheckpointStore
	Broker      BrokerService
}

// Server carries the dependencies required by handlers. Tests construct
// it directly; production code goes through NewHandler.
type Server struct {
	Service     LaunchService
	Checkpoints CheckpointStore
	Broker      BrokerService
}

// NewHandler builds the http.Handler serving every route owned by the
// api package. The daemon package layers /health on top of this.
func NewHandler(deps Deps) http.Handler {
	s := &Server{
		Service:     deps.Service,
		Checkpoints: deps.Checkpoints,
		Broker:      deps.Broker,
	}
	mux := http.NewServeMux()
	s.registerSessionRoutes(mux)
	s.registerCheckpointRoutes(mux)
	s.registerBrokerRoutes(mux)
	return mux
}
