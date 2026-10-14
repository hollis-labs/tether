package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hollis-labs/tether/internal/execpolicy"
)

// ExecutionPolicyService is the optional execution-control surface of the
// launch service (CW-20261011-0015). Routes are registered only when the
// service implements it.
type ExecutionPolicyService interface {
	ExecutionStatus() (execpolicy.Status, error)
	SetExecutionPolicy(execpolicy.Policy) (execpolicy.Status, error)
}

func (s *Server) registerExecutionRoutes(router *http.ServeMux) {
	svc, ok := s.Service.(ExecutionPolicyService)
	if !ok {
		return
	}
	if capSvc, ok := s.Service.(interface {
		Capacity() (execpolicy.CapacityView, error)
	}); ok {
		router.HandleFunc("/capacity", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
				return
			}
			v, err := capSvc.Capacity()
			if err != nil {
				writeError(w, http.StatusInternalServerError, CodeInternalError, "read capacity: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, v)
		})
	}
	router.HandleFunc("/execution/policy", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			st, err := svc.ExecutionStatus()
			if err != nil {
				writeError(w, http.StatusInternalServerError, CodeInternalError, "read execution policy: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, st)
		case http.MethodPut:
			var p execpolicy.Policy
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&p); err != nil {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid execution policy body: "+err.Error())
				return
			}
			if err := p.Validate(); err != nil {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
				return
			}
			var st execpolicy.Status
			var err error
			if cs, ok := s.Service.(interface {
				SetExecutionPolicyContext(context.Context, execpolicy.Policy) (execpolicy.Status, error)
			}); ok {
				st, err = cs.SetExecutionPolicyContext(r.Context(), p)
			} else {
				st, err = svc.SetExecutionPolicy(p)
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, CodeInternalError, "set execution policy: "+err.Error())
				return
			}
			writeJSON(w, http.StatusOK, st)
		default:
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		}
	})
}
