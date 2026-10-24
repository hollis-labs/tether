package mcptransport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/callcontext"
)

func TestWithTelemetryHints(t *testing.T) {
	req := func(label, session string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if label != "" {
			r.Header.Set("X-Tether-Client-Label", label)
		}
		if session != "" {
			r.Header.Set("Mcp-Session-Id", session)
		}
		return r
	}

	r := req("tether-sysadmin", "SESSION-HANDLE-123")
	ctx := withTelemetryHints(r.Context(), r)
	if got := callcontext.ClientLabel(ctx); got != "tether-sysadmin" {
		t.Fatalf("label = %q", got)
	}
	hash := callcontext.MCPSession(ctx)
	if hash == "" || len(hash) != 12 || strings.Contains(hash, "SESSION") {
		t.Fatalf("session must be a short hash, not the handle: %q", hash)
	}
	// Stable per handle, different across handles.
	r2 := req("", "SESSION-HANDLE-123")
	if got := callcontext.MCPSession(withTelemetryHints(r2.Context(), r2)); got != hash {
		t.Fatalf("hash not stable: %q vs %q", got, hash)
	}
	r3 := req("", "OTHER")
	if got := callcontext.MCPSession(withTelemetryHints(r3.Context(), r3)); got == hash {
		t.Fatal("different handles must hash differently")
	}

	// An invalid label is dropped, never repaired or truncated into something valid.
	bad := req("not valid;label", "")
	if got := callcontext.ClientLabel(withTelemetryHints(bad.Context(), bad)); got != "" {
		t.Fatalf("invalid label kept: %q", got)
	}
	none := req("", "")
	c := withTelemetryHints(none.Context(), none)
	if callcontext.ClientLabel(c) != "" || callcontext.MCPSession(c) != "" {
		t.Fatal("absent hints must stay absent")
	}
}
