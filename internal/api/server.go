package api

import (
	"net/http"
)

// Deps bundles everything the api handlers need at construction time.
// The daemon package builds this and calls NewHandler to get an
// http.Handler back.
type Deps struct {
	// Service is required for any /sessions path. When nil, those
	// routes return 404 instead of panicking.
	Service LaunchService
}

// Server carries the dependencies required by handlers. Tests construct
// it directly; production code goes through NewHandler.
type Server struct {
	Service LaunchService
}

// NewHandler builds the http.Handler serving every route owned by the
// api package. The daemon package layers /health on top of this.
func NewHandler(deps Deps) http.Handler {
	s := &Server{Service: deps.Service}
	mux := http.NewServeMux()
	s.registerSessionRoutes(mux)
	return mux
}
