package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/hollis-labs/tether/internal/catalogapply"
)

type CatalogBreakglassRequest struct {
	Principal  string `json:"principal"`
	Reason     string `json:"reason"`
	For        string `json:"for"`
	Passphrase string `json:"passphrase"`
	End        bool   `json:"end"`
}

func (s *Server) handleCatalogApprovers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
		return
	}
	svc, ok := s.CatalogApply.(interface {
		Approvers() (catalogapply.ApproverView, error)
	})
	if !ok {
		writeError(w, 503, CodeInternalError, "approver visibility unavailable")
		return
	}
	view, err := svc.Approvers()
	if err != nil {
		writeError(w, 409, CodeConflict, err.Error())
		return
	}
	writeJSON(w, 200, view)
}
func (s *Server) handleCatalogBreakglass(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if !catalogApprover(r) {
		writeError(w, 403, CodeForbidden, "breakglass requires the local operator credential")
		return
	}
	var req CatalogBreakglassRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		writeError(w, 400, CodeInvalidRequest, "invalid breakglass request")
		return
	}
	duration, err := time.ParseDuration(req.For)
	if !req.End && err != nil {
		writeError(w, 400, CodeInvalidRequest, "invalid duration")
		return
	}
	svc, ok := s.CatalogApply.(interface {
		Breakglass(string, string, time.Duration, string, bool) (*catalogapply.BreakglassWindow, error)
	})
	if !ok {
		writeError(w, 503, CodeInternalError, "breakglass unavailable")
		return
	}
	window, err := svc.Breakglass(req.Principal, req.Reason, duration, req.Passphrase, req.End)
	if err != nil {
		writeError(w, 403, CodeForbidden, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"breakglass": window})
}
