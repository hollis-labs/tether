package mcpadapter

import (
	"context"
	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
)

const (
	// ProvenanceMetaKey is the reserved metadata key for Tether provenance.
	ProvenanceMetaKey = "tether.provenance"
	// ProvenanceSchemaVersion is the current provenance schema version.
	ProvenanceSchemaVersion = 1
)

// WorkstreamResolver resolves the current workstream ID for a session ID.
// It returns an empty string without error if the session has no workstream assigned.
type WorkstreamResolver func(ctx context.Context, sessionID string) (string, error)

// ProvenanceEnvelope defines the wire format of tether.provenance in params._meta.
type ProvenanceEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	WorkstreamID  string `json:"workstream_id,omitempty"`
}

// SetWorkstreamResolver configures the resolver used to look up a session's
// current workstream snapshot at proxy forwarding time.
func (r *ProxyRouter) SetWorkstreamResolver(fn WorkstreamResolver) {
	r.workstreamResolver = fn
}

// SetLogger configures the logger used by the router for bounded diagnostics.
func (r *ProxyRouter) SetLogger(l *slog.Logger) {
	r.logger = l
}

// applyProvenanceMeta enforces Tether provenance at the outbound proxy forwarding seam.
//
// Behavior:
//   - If an inbound request carries params._meta["tether.provenance"], it is always
//     replaced or removed so client-invented stamps are never forwarded.
//   - If no Tether session is configured in ctx, the envelope is omitted and any
//     client-supplied stamp is stripped.
//   - If a Tether session is configured, the session's workstream snapshot is resolved
//     once for this forwarding attempt.
//   - If workstream lookup fails, a warning is logged, the envelope is omitted, any
//     client-supplied stamp is stripped, and the content call is NOT failed.
//   - If lookup succeeds, tether.provenance is stamped with schema_version=1,
//     session_id, and (if assigned) workstream_id.
//   - Unrelated metadata (e.g. progressToken, trace context) and ordinary arguments
//     are preserved untouched.
func (r *ProxyRouter) applyProvenanceMeta(ctx context.Context, req mcp.CallToolRequest) mcp.CallToolRequest {
	sessionID := sessionIDFromContext(ctx)
	var prov *ProvenanceEnvelope

	if sessionID != "" {
		var wsID string
		var lookupFailed bool
		if r.workstreamResolver != nil {
			var err error
			wsID, err = r.workstreamResolver(ctx, sessionID)
			if err != nil {
				lookupFailed = true
				logger := r.logger
				if logger == nil {
					logger = slog.Default()
				}
				logger.WarnContext(ctx, "failed to resolve session workstream for provenance",
					"session_id", sessionID,
					"error", err,
				)
			}
		}
		if !lookupFailed {
			prov = &ProvenanceEnvelope{
				SchemaVersion: ProvenanceSchemaVersion,
				SessionID:     sessionID,
				WorkstreamID:  wsID,
			}
		}
	}

	var existingFields map[string]any
	if req.Params.Meta != nil && req.Params.Meta.AdditionalFields != nil {
		existingFields = req.Params.Meta.AdditionalFields
	}

	if prov != nil {
		provMap := map[string]any{
			"schema_version": prov.SchemaVersion,
			"session_id":     prov.SessionID,
		}
		if prov.WorkstreamID != "" {
			provMap["workstream_id"] = prov.WorkstreamID
		}

		fields := make(map[string]any, len(existingFields)+1)
		for k, v := range existingFields {
			if k != ProvenanceMetaKey {
				fields[k] = v
			}
		}
		fields[ProvenanceMetaKey] = provMap

		if req.Params.Meta == nil {
			req.Params.Meta = &mcp.Meta{}
		}
		req.Params.Meta.AdditionalFields = fields
		return req
	}

	// Provenance is omitted. If incoming request carries ProvenanceMetaKey, strip it.
	if existingFields != nil {
		if _, hasProv := existingFields[ProvenanceMetaKey]; hasProv {
			fields := make(map[string]any, len(existingFields))
			for k, v := range existingFields {
				if k != ProvenanceMetaKey {
					fields[k] = v
				}
			}
			if len(fields) > 0 {
				req.Params.Meta.AdditionalFields = fields
			} else {
				req.Params.Meta.AdditionalFields = nil
				if req.Params.Meta.ProgressToken == nil {
					req.Params.Meta = nil
				}
			}
		}
	}

	return req
}

// ExtractProvenanceMeta reads and parses tether.provenance from req.Params.Meta, if present.
func ExtractProvenanceMeta(req mcp.CallToolRequest) *ProvenanceEnvelope {
	if req.Params.Meta == nil || req.Params.Meta.AdditionalFields == nil {
		return nil
	}
	raw, ok := req.Params.Meta.AdditionalFields[ProvenanceMetaKey]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case *ProvenanceEnvelope:
		return v
	case ProvenanceEnvelope:
		return &v
	case map[string]any:
		env := &ProvenanceEnvelope{}
		if sv, ok := v["schema_version"].(int); ok {
			env.SchemaVersion = sv
		} else if svf, ok := v["schema_version"].(float64); ok {
			env.SchemaVersion = int(svf)
		}
		if sid, ok := v["session_id"].(string); ok {
			env.SessionID = sid
		}
		if wid, ok := v["workstream_id"].(string); ok {
			env.WorkstreamID = wid
		}
		return env
	default:
		return nil
	}
}
