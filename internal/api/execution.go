package api

import (
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
			st, err := svc.SetExecutionPolicy(p)
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
