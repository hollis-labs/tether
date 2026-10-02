package mcpadapter

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/callcontext"
	"log/slog"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	ContextMetaKey       = "tether.context"
	ContextSchemaVersion = 2
	// ProvenanceMetaKey is the reserved metadata key for Tether provenance.
	ProvenanceMetaKey = "tether.provenance"
	// ProvenanceSchemaVersion is the current provenance schema version.
	ProvenanceSchemaVersion = 1
)

type ContextEnvelope struct {
	SchemaVersion int `json:"schema_version"`
	callcontext.Snapshot
}

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
func (r *ProxyRouter) applyProvenanceMeta(ctx context.Context, params *mcpsdk.CallToolParams) {
	r.applyContextMeta(ctx, params)
	snapshot, hasSnapshot := callcontext.FromContext(ctx)
	sessionID := sessionIDFromContext(ctx)
	if hasSnapshot && snapshot.Verified {
		sessionID = snapshot.SessionID
	}
	var prov *ProvenanceEnvelope

	if sessionID != "" {
		var wsID string
		var lookupFailed bool
		if hasSnapshot && snapshot.Verified {
			wsID = snapshot.WorkstreamID
		} else if r.workstreamResolver != nil {
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

	existingFields := map[string]any(params.Meta)

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

		params.Meta = mcpsdk.Meta(fields)
		return
	}

	// Provenance is omitted. If incoming request carries ProvenanceMetaKey, strip it.
	if _, hasProv := existingFields[ProvenanceMetaKey]; hasProv {
		fields := make(map[string]any, len(existingFields))
		for k, v := range existingFields {
			if k != ProvenanceMetaKey {
				fields[k] = v
			}
		}
		if len(fields) > 0 {
			params.Meta = mcpsdk.Meta(fields)
		} else {
			params.Meta = nil
		}
	}
}

// applyContextMeta strips all incoming trusted context and stamps only the
// daemon-resolved snapshot. Legacy provenance remains correlation-only.
func (r *ProxyRouter) applyContextMeta(ctx context.Context, params *mcpsdk.CallToolParams) {
	fields := make(map[string]any, len(params.Meta)+1)
	for k, v := range params.Meta {
		if k != ContextMetaKey {
			fields[k] = v
		}
	}
	if snapshot, ok := callcontext.FromContext(ctx); ok && snapshot.Verified && snapshot.Source == "daemon" && snapshot.SessionID != "" {
		raw, _ := json.Marshal(ContextEnvelope{SchemaVersion: ContextSchemaVersion, Snapshot: snapshot})
		var stamp map[string]any
		_ = json.Unmarshal(raw, &stamp)
		fields[ContextMetaKey] = stamp
	}
	if len(fields) == 0 {
		params.Meta = nil
	} else {
		params.Meta = mcpsdk.Meta(fields)
	}
}

func ExtractContextMeta(meta map[string]any) *ContextEnvelope {
	raw, ok := meta[ContextMetaKey]
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var envelope ContextEnvelope
	if json.Unmarshal(encoded, &envelope) != nil {
		return nil
	}
	return &envelope
}

// ExtractProvenanceMeta reads and parses tether.provenance from meta, if present.
func ExtractProvenanceMeta(meta map[string]any) *ProvenanceEnvelope {
	if meta == nil {
		return nil
	}
	raw, ok := meta[ProvenanceMetaKey]
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
