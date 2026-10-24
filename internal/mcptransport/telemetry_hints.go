package mcptransport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/hollis-labs/tether/internal/callcontext"
)

// withTelemetryHints attaches two observation-only hints to the request context
// (CW-20261024-0048). Neither grants anything and neither is read by policy.
//
//   - X-Tether-Client-Label: the caller's self-declared name. Operator-token
//     clients share one verified principal, so without this they are one blob in
//     telemetry. It is a claim; an invalid value is dropped, not repaired.
//   - Mcp-Session-Id: reduced to a short hash so calls from one connection can
//     be grouped (and linked to the search that preceded them) without storing
//     the handle itself.
func withTelemetryHints(ctx context.Context, r *http.Request) context.Context {
	if label := r.Header.Get("X-Tether-Client-Label"); callcontext.ValidClientLabel(label) {
		ctx = callcontext.WithClientLabel(ctx, label)
	}
	if id := r.Header.Get("Mcp-Session-Id"); id != "" {
		sum := sha256.Sum256([]byte(id))
		ctx = callcontext.WithMCPSession(ctx, hex.EncodeToString(sum[:6]))
	}
	return ctx
}
