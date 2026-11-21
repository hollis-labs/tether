package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// CorrelationRegisterRequest contains descriptive external-launch facts only.
// The daemon owns the Registry credential; callers cannot supply an identity,
// Registry authority, token or token JTI.
type CorrelationRegisterRequest struct {
	URN               string `json:"urn"`
	Session           string `json:"session"`
	RunID             string `json:"run_id,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	WorkstreamID      string `json:"workstream_id,omitempty"`
	ManifestDigest    string `json:"manifest_digest,omitempty"`
	ExportReceiptID   string `json:"export_receipt_id,omitempty"`
	ActivitySHA256    string `json:"activity_sha256,omitempty"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	// Host, DisplayName and Summary are descriptive and local only (shown by
	// tether_agents_live); they are never sent to the Registry.
	Host        string `json:"host,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Summary     string `json:"summary,omitempty"`
	// ActivitySummary is a heartbeat's one-line activity; when set it takes
	// precedence over Summary.
	ActivitySummary string `json:"activity_summary,omitempty"`
}

type CorrelationRegisterResponse struct {
	Queued bool   `json:"queued"`
	Reason string `json:"reason,omitempty"`
}

type CorrelationRegistrar interface {
	RegisterExternalCorrelation(context.Context, CorrelationRegisterRequest) (CorrelationRegisterResponse, error)
}

// Validate rejects malformed facts before any queue or credential IO.
func (in CorrelationRegisterRequest) Validate() error {
	if !strings.HasPrefix(in.URN, "msg://agent/") || strings.TrimPrefix(in.URN, "msg://agent/") == "" || in.Session == "" {
		return errors.New("agent urn and session required")
	}
	for _, v := range []string{in.URN, in.Session, in.RunID, in.TaskID, in.WorkstreamID, in.ManifestDigest, in.ExportReceiptID, in.ActivitySHA256, in.ProviderSessionID, in.Host, in.DisplayName, in.Summary, in.ActivitySummary} {
		if len(v) > 2048 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\x00\r\n\t") {
			return errors.New("invalid correlation fact")
		}
	}
	if in.WorkstreamID != "" && !store.IsRegistryWorkstreamID(in.WorkstreamID) {
		return errors.New("canonical Registry workstream required")
	}
	if in.ActivitySHA256 != "" {
		b, err := hex.DecodeString(in.ActivitySHA256)
		if err != nil || len(b) != 32 {
			return errors.New("activity_sha256 must be 64 hexadecimal characters")
		}
	}
	return nil
}

func (s *Server) registerCorrelationRoutes(router *http.ServeMux) {
	if s.Correlations == nil {
		return
	}
	router.HandleFunc("POST /correlation/register", s.handleCorrelationRegister)
}

func (s *Server) handleCorrelationRegister(w http.ResponseWriter, r *http.Request) {
	// Reusing the launcher's privileged Registry credential is local operator
	// custody, never a new way for session or remote-device tokens to mint facts.
	p, _ := identity.FromContext(r.Context())
	if !identity.IsOperatorCredential(p) {
		writeError(w, 403, CodeForbidden, "local operator credential required")
		return
	}
	var in CorrelationRegisterRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, 400, CodeInvalidRequest, "invalid correlation request")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, 400, CodeInvalidRequest, "one JSON object required")
		return
	}
	if err := in.Validate(); err != nil {
		writeError(w, 400, CodeInvalidRequest, err.Error())
		return
	}
	out, err := s.Correlations.RegisterExternalCorrelation(r.Context(), in)
	if err != nil {
		writeError(w, 500, CodeInternalError, "correlation registration unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}
