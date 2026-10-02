package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/hollis-labs/tether/internal/app/proxyevents"
	"github.com/hollis-labs/tether/internal/callcontext"
)

// ProxyEventStore remains the narrow composition seam for existing callers.
type ProxyEventStore = proxyevents.Store

// ProxyEventDTO is an application-owned wire contract, not a storage-row alias.
type ProxyEventDTO = proxyevents.ProxyEventDTO
type ProxyEventListResponse = proxyevents.ProxyEventListResponse
type ProxyEventIngestRequest proxyevents.ProxyEventIngestRequest

const (
	ProxyEventPhaseStart    = proxyevents.ProxyEventPhaseStart
	ProxyEventPhaseEnd      = proxyevents.ProxyEventPhaseEnd
	maxProxyEventBodyBytes  = 64 << 10
	MaxProxyEventErrorBytes = proxyevents.MaxProxyEventErrorBytes
)

func TruncateProxyEventError(s string) string { return proxyevents.TruncateProxyEventError(s) }

func (s *Server) registerProxyEventRoutes(router *http.ServeMux) {
	if s.ProxyEvents == nil {
		return
	}
	router.HandleFunc("/proxy/events", s.handleProxyEvents)
}

func (s *Server) handleProxyEvents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListProxyEvents(w, r)
	case http.MethodPost:
		s.handleIngestProxyEvent(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

// handleListProxyEvents serves GET /proxy/events.
//
// Query params (all optional):
//
//	?server=hadron          – filter by exact server ID
//	?tool_name=hadron_      – filter by tool name prefix
//	?session_id=abc         – filter by session ID
//	?limit=50               – max results (default 100, max 500)
//	?since=2026-01-01T00:00:00Z – RFC3339 lower bound on timestamp
//	?errors_only=true       – only return failed calls
func (s *Server) handleListProxyEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := proxyevents.Query{
		ServerID:  q.Get("server"),
		ToolName:  q.Get("tool_name"),
		SessionID: q.Get("session_id"),
	}

	if lim := q.Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil {
			f.Limit = n
		}
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			f.Since = t
		}
	}
	if q.Get("errors_only") == "true" {
		f.ErrorsOnly = true
	}

	evs, err := proxyevents.QueryRecords(s.ProxyEvents, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "query proxy events: "+err.Error())
		return
	}

	dtos := make([]ProxyEventDTO, len(evs))
	for i, ev := range evs {
		dtos[i] = proxyevents.ToDTO(ev)
	}
	writeJSON(w, http.StatusOK, ProxyEventListResponse{Events: dtos, Count: len(dtos)})
}

// handleIngestProxyEvent decodes the HTTP body and maps application failures.
func (s *Server) handleIngestProxyEvent(w http.ResponseWriter, r *http.Request) {
	var req ProxyEventIngestRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyEventBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
		return
	}
	var sessionExists func(string) error
	if s.Service != nil {
		sessionExists = func(id string) error { _, err := s.Service.GetSession(id); return err }
	}
	operation := proxyevents.New(s.ProxyEvents, s.Bus, sessionExists)
	err := operation.Ingest(r.Context(), proxyevents.ProxyEventIngestRequest(req), func() callcontext.Snapshot { return ResolveCallerContext(r.Context(), s.Service, s.Registry) })
	if err != nil {
		var failure *proxyevents.Error
		if errors.As(err, &failure) {
			status := http.StatusInternalServerError
			switch failure.Code {
			case "bad_request":
				status = http.StatusBadRequest
			case "not_found":
				status = http.StatusNotFound
			}
			writeError(w, status, failure.Code, failure.Message)
		} else {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}
