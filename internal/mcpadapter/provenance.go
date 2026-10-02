package mcpadapter

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/callcontext"
	"log/slog"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// ProvenanceMetaKey is the reserved metadata key for Tether provenance.
	ProvenanceMetaKey = "tether.provenance"
	// ProvenanceSchemaVersion is the current provenance schema version.
	ProvenanceSchemaVersion = 2
)

// ProvenanceEnvelope defines the wire format of tether.provenance in params._meta.
type ProvenanceEnvelope struct {
	SchemaVersion int `json:"schema_version"`
	callcontext.Snapshot
}

// SetLogger configures the logger used by the router for bounded diagnostics.
func (r *ProxyRouter) SetLogger(l *slog.Logger) {
	r.logger = l
}

// applyProvenanceMeta enforces Tether provenance at the outbound proxy forwarding seam.
//
// Client-provided provenance is always removed. Only a verified daemon
// snapshot is stamped. Ordinary arguments and unrelated metadata are preserved.
func (r *ProxyRouter) applyProvenanceMeta(ctx context.Context, params *mcpsdk.CallToolParams) {
	snapshot, ok := callcontext.FromContext(ctx)
	fields := make(map[string]any, len(params.Meta)+1)
	for k, v := range params.Meta {
		if k != ProvenanceMetaKey {
			fields[k] = v
		}
	}
	if ok && snapshot.Verified && snapshot.SessionID != "" {
		raw, _ := json.Marshal(ProvenanceEnvelope{SchemaVersion: ProvenanceSchemaVersion, Snapshot: snapshot})
		var stamp map[string]any
		_ = json.Unmarshal(raw, &stamp)
		fields[ProvenanceMetaKey] = stamp
	}
	if len(fields) == 0 {
		params.Meta = nil
	} else {
		params.Meta = mcpsdk.Meta(fields)
	}
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
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var env ProvenanceEnvelope
		if json.Unmarshal(raw, &env) != nil {
			return nil
		}
		return &env
	default:
		return nil
	}
}
