package callcontext

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestValidClientLabel(t *testing.T) {
	for _, ok := range []string{"tether", "tether-sysadmin", "nanite:architect", "agent@host/role_1.v2", strings.Repeat("a", 64)} {
		if !ValidClientLabel(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", 65), "has space", "semi;colon", "new\nline", "quote\"", "ünicode", "tab\t", "a,b", "a=b"} {
		if ValidClientLabel(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestWithTelemetryHints(t *testing.T) {
	hdr := func(label, session string) http.Header {
		h := http.Header{}
		if label != "" {
			h.Set("X-Tether-Client-Label", label)
		}
		if session != "" {
			h.Set("Mcp-Session-Id", session)
		}
		return h
	}
	ctx := WithTelemetryHints(context.Background(), hdr("tether-sysadmin", "SESSION-HANDLE-123"))
	if got := ClientLabel(ctx); got != "tether-sysadmin" {
		t.Fatalf("label = %q", got)
	}
	hash := MCPSession(ctx)
	if len(hash) != 12 || strings.Contains(hash, "SESSION") {
		t.Fatalf("session must be a short hash, not the handle: %q", hash)
	}
	if MCPSession(WithTelemetryHints(context.Background(), hdr("", "SESSION-HANDLE-123"))) != hash {
		t.Fatal("hash not stable for the same handle")
	}
	if MCPSession(WithTelemetryHints(context.Background(), hdr("", "OTHER"))) == hash {
		t.Fatal("different handles must hash differently")
	}
	// An invalid label is dropped, never repaired; absent hints stay absent.
	if ClientLabel(WithTelemetryHints(context.Background(), hdr("not valid;label", ""))) != "" {
		t.Fatal("invalid label kept")
	}
	empty := WithTelemetryHints(context.Background(), nil)
	if ClientLabel(empty) != "" || MCPSession(empty) != "" {
		t.Fatal("nil headers must add nothing")
	}
}
