package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /health carries the daemon's own view of control-plane protection when the
// server has one, and omits the field otherwise, so `mux doctor` can tell a
// daemon that reports it from one that does not (CW-20261001-0142).
func TestHandleHealth_SandboxProtect(t *testing.T) {
	get := func(s *Server) (Health, string) {
		rr := httptest.NewRecorder()
		s.handleHealth(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
		var h Health
		if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
			t.Fatalf("decode %q: %v", rr.Body.String(), err)
		}
		return h, rr.Body.String()
	}

	h, raw := get(&Server{})
	if h.SandboxProtect != nil || strings.Contains(raw, "sandbox_protect") {
		t.Fatalf("a server with no protection view reported one: %s", raw)
	}

	want := SandboxProtectHealth{Enabled: true, Reason: "on", Codex: "not protected", CodexReason: "not protected (CW-20261001-0230): codex runs as before", BwrapChecked: true, BwrapError: "uid map: Permission denied"}
	h, raw = get(&Server{SandboxProtect: func() *SandboxProtectHealth { return &want }})
	if h.SandboxProtect == nil || *h.SandboxProtect != want {
		t.Fatalf("sandbox_protect = %+v, want %+v (%s)", h.SandboxProtect, want, raw)
	}
	for _, field := range []string{`"enabled":true`, `"bwrap_checked":true`, `"bwrap_error":"uid map: Permission denied"`, `"codex":"not protected"`, `"codex_reason":"not protected (CW-20261001-0230): codex runs as before"`} {
		if !strings.Contains(raw, field) {
			t.Errorf("/health body lacks %s: %s", field, raw)
		}
	}
}
