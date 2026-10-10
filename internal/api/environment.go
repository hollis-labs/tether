package api

import "net/http"

func (s *Server) registerEnvironmentRoutes(router *http.ServeMux) {
	if s.EnvironmentStream == nil {
		return
	}
	router.HandleFunc("/environment/snapshot", func(w http.ResponseWriter, r *http.Request) { s.EnvironmentStream.Snapshot(w, r, "") })
	router.HandleFunc("/environment/events", func(w http.ResponseWriter, r *http.Request) { s.EnvironmentStream.Stream(w, r, "") })
	router.HandleFunc("/sessions/{id}/snapshot", func(w http.ResponseWriter, r *http.Request) { s.EnvironmentStream.Snapshot(w, r, r.PathValue("id")) })
	router.HandleFunc("/sessions/{id}/stream", func(w http.ResponseWriter, r *http.Request) { s.EnvironmentStream.Stream(w, r, r.PathValue("id")) })
}
