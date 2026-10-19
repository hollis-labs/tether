package api

// catalog_apply.go — plan / approve / apply / rollback for catalog changes
// (CW-20261012-0124). The daemon owns the writes; the MCP tools are thin
// callers. Approval is a separate act with a human-only credential: it is
// refused for any session principal, any remote caller, and any caller
// without the operator's approval passphrase (internal/catalogapply).
//
//	POST /catalog/plans                 plan a bundle, or a rollback to a backup
//	GET  /catalog/plans/{id}            read a plan
//	POST /catalog/plans/{id}/approve    local operator only, passphrase + digest
//	POST /catalog/plans/{id}/apply      install an approved plan
//	GET  /catalog/backups               list catalog backups

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/catalogcheck"
	"github.com/hollis-labs/tether/internal/identity"
)

// CatalogApplyService is the seam over catalogapply.Engine.
type CatalogApplyService interface {
	PlanBundle(ctx context.Context, b catalogapply.Bundle, opt catalogapply.Options) (*catalogapply.Plan, error)
	PlanRollback(ctx context.Context, backupID string, opt catalogapply.Options) (*catalogapply.Plan, error)
	Get(id string) (*catalogapply.Plan, bool)
	Approve(req catalogapply.ApprovalRequest) (*catalogapply.Plan, error)
	Apply(ctx context.Context, planID string) (*catalogapply.Result, error)
	Backups() ([]catalogapply.BackupInfo, error)
	ListFiles() ([]catalogapply.FileInfo, error)
	ReadFile(rel string) (string, error)
	History(limit int) ([]catalogapply.AuditRecord, error)
	ValidateBundle(files []catalogapply.File, opt catalogapply.Options) (catalogcheck.Report, error)
}

// catalogWriteScope is the session credential scope that lets a launched
// agent plan and apply catalog changes.
const catalogWriteScope = "catalog.write"

func (s *Server) registerCatalogApplyRoutes(router *http.ServeMux) {
	if s.CatalogApply == nil {
		return
	}
	router.HandleFunc("/catalog/plans", s.handleCatalogPlans)
	router.HandleFunc("/catalog/plans/team-activation", s.handleTeamActivationPlan)
	router.HandleFunc("/catalog/plans/", s.handleCatalogPlanItem)
	router.HandleFunc("/catalog/backups", s.handleCatalogBackups)
	router.HandleFunc("/catalog/validate", s.handleCatalogValidate)
	router.HandleFunc("/catalog/files", s.handleCatalogFiles)
	router.HandleFunc("/catalog/audit", s.handleCatalogAudit)
}

// CatalogPlanRequest is the POST /catalog/plans body. Exactly one of Files or
// RollbackOf is set.
type CatalogPlanRequest struct {
	Name       string              `json:"name"`
	TaskID     string              `json:"task_id,omitempty"`
	Files      []catalogapply.File `json:"files,omitempty"`
	RollbackOf string              `json:"rollback_of,omitempty"`
}

// CatalogApproveRequest is the POST /catalog/plans/{id}/approve body.
type CatalogApproveRequest struct {
	Digest        string `json:"digest"`
	Passphrase    string `json:"passphrase"`
	AckPrivileged bool   `json:"ack_privileged,omitempty"`
	AllowRunning  bool   `json:"allow_running,omitempty"`
}

// catalogWriter reports whether the caller may plan or apply. A session
// principal must hold catalog.write; a session credential minted for another
// purpose cannot reach these routes by speaking HTTP directly.
func catalogWriter(r *http.Request) bool {
	p, ok := identity.FromContext(r.Context())
	if !ok {
		return true // no verified principal: local trust, as for the rest of the local API
	}
	if p.Kind == "session" || p.Kind == "agent" || p.TokenID != "" {
		return identity.ScopeGranted(p.Scopes, catalogWriteScope)
	}
	return true
}

// catalogApprover reports whether the caller may even attempt an approval:
// local socket only, and never as a session, device or service principal.
// This is a first gate, not the proof — the passphrase is.
func catalogApprover(r *http.Request) bool {
	if identity.IsRemote(r.Context()) || !identity.LocalConnection(r.Context()) {
		return false
	}
	p, ok := identity.FromContext(r.Context())
	if !ok {
		return true
	}
	return identity.IsOperatorCredential(p)
}

func (s *Server) handleCatalogPlans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if !catalogWriter(r) {
		writeError(w, http.StatusForbidden, CodeForbidden, "planning a catalog change needs the "+catalogWriteScope+" scope")
		return
	}
	var req CatalogPlanRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	opt := catalogapply.Options{ProtectedPaths: s.callerDefinitionPaths(r), Caller: s.callerOf(r)}
	var (
		plan *catalogapply.Plan
		err  error
	)
	switch {
	case req.RollbackOf != "" && len(req.Files) > 0:
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "send files or rollback_of, not both")
		return
	case req.RollbackOf != "":
		plan, err = s.CatalogApply.PlanRollback(r.Context(), req.RollbackOf, opt)
	default:
		plan, err = s.CatalogApply.PlanBundle(r.Context(), catalogapply.Bundle{Name: req.Name, TaskID: req.TaskID, Files: req.Files}, opt)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleCatalogPlanItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/catalog/plans/")
	id, action, _ := strings.Cut(rest, "/")
	if id == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "plan id required")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		plan, ok := s.CatalogApply.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, CodeNotFound, "plan not found or expired")
			return
		}
		writeJSON(w, http.StatusOK, plan)
	case action == "approve" && r.Method == http.MethodPost:
		s.handleCatalogApprove(w, r, id)
	case action == "apply" && r.Method == http.MethodPost:
		if !catalogWriter(r) {
			writeError(w, http.StatusForbidden, CodeForbidden, "applying a catalog change needs the "+catalogWriteScope+" scope")
			return
		}
		res, err := s.CatalogApply.Apply(r.Context(), id)
		if err != nil {
			status := http.StatusConflict
			if res == nil && strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			if res != nil {
				// A failed install that was restored still reports what happened.
				writeJSON(w, http.StatusConflict, map[string]any{"error": ErrorDetail{Code: CodeConflict, Message: err.Error()}, "result": res})
				return
			}
			writeError(w, status, CodeConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleCatalogApprove(w http.ResponseWriter, r *http.Request, id string) {
	if !catalogApprover(r) {
		writeError(w, http.StatusForbidden, CodeForbidden, "a plan is approved by the local operator on the daemon's own socket, not by a session, a device or a remote caller")
		return
	}
	var req CatalogApproveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	plan, err := s.CatalogApply.Approve(catalogapply.ApprovalRequest{
		PlanID: id, Digest: req.Digest, Passphrase: req.Passphrase,
		AckPrivileged: req.AckPrivileged, AllowRunning: req.AllowRunning,
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, plan)
	case errors.Is(err, catalogapply.ErrBadPassphrase), errors.Is(err, catalogapply.ErrNoPassphrase), errors.Is(err, catalogapply.ErrLockedOut):
		writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
	default:
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	}
}

func (s *Server) handleCatalogBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	backups, err := s.CatalogApply.Backups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	if backups == nil {
		backups = []catalogapply.BackupInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
}

// CatalogValidateRequest is the POST /catalog/validate body. With no files the
// live catalog is validated; with files, the catalog as it would be with them
// applied. Nothing is stored or written.
type CatalogValidateRequest struct {
	Files []catalogapply.File `json:"files,omitempty"`
}

func (s *Server) handleCatalogValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	var req CatalogValidateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	report, err := s.CatalogApply.ValidateBundle(req.Files, catalogapply.Options{})
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// callerOf names the caller for the audit trail.
func (s *Server) callerOf(r *http.Request) catalogapply.Caller {
	p, ok := identity.FromContext(r.Context())
	if !ok {
		return catalogapply.Caller{Principal: "local"}
	}
	c := catalogapply.Caller{Principal: p.Kind + ":" + p.ID, SessionID: p.SessionID}
	if p.SessionID != "" && s.Service != nil {
		if row, err := s.Service.GetSession(p.SessionID); err == nil && row != nil {
			c.AgentID = row.LogicalAgentID
		}
	}
	return c
}

// handleCatalogFiles lists the raw-readable catalog files, or with ?path=
// returns one file's content.
func (s *Server) handleCatalogFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if path := r.URL.Query().Get("path"); path != "" {
		content, err := s.CatalogApply.ReadFile(path)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": path, "content": content})
		return
	}
	files, err := s.CatalogApply.ListFiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleCatalogAudit lists applied changes, newest first.
func (s *Server) handleCatalogAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	recs, err := s.CatalogApply.History(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": recs})
}

// callerDefinitionPaths names the catalog files that define the calling
// session's own agent and launch, which a plan from that session may not
// change.
func (s *Server) callerDefinitionPaths(r *http.Request) []string {
	p, ok := identity.FromContext(r.Context())
	if !ok || p.Kind != "session" || p.SessionID == "" || s.Service == nil {
		return nil
	}
	row, err := s.Service.GetSession(p.SessionID)
	if err != nil || row == nil {
		return nil
	}
	var paths []string
	if row.LogicalAgentID != "" {
		paths = append(paths, "agents/"+row.LogicalAgentID+".yaml", "agents/"+row.LogicalAgentID+".yml")
	}
	if row.LaunchID != "" {
		paths = append(paths, "launches/"+row.LaunchID+".yaml", "launches/"+row.LaunchID+".yml")
	}
	return paths
}
