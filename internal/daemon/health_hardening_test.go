package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func healthBody(t *testing.T, s *Server) (Health, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handleHealth(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	var h Health
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	return h, rr.Body.String()
}

// /health reports the launch-hardening state the daemon decided
// (CW-20261001-0227), so `tether doctor` can say what tetherd does.
func TestHealth_ReportsLaunchHardening(t *testing.T) {
	s := &Server{Hardening: func() *HealthHardening {
		return &HealthHardening{ClaudeStrictMCP: false, ClaudeStrictMCPReason: "DISABLED by test"}
	}}
	h, raw := healthBody(t, s)
	if h.Hardening == nil || h.Hardening.ClaudeStrictMCP || h.Hardening.ClaudeStrictMCPReason != "DISABLED by test" {
		t.Fatalf("hardening = %+v", h.Hardening)
	}
	if !strings.Contains(raw, `"claude_strict_mcp":false`) {
		t.Fatalf("a false switch must be present in the body, not omitted: %s", raw)
	}
}

// A daemon wired without it, and an older one, omit the field, which is how
// doctor tells "predates" from "off".
func TestHealth_OmitsHardeningWhenUnset(t *testing.T) {
	h, raw := healthBody(t, &Server{})
	if h.Hardening != nil || strings.Contains(raw, "hardening") {
		t.Fatalf("hardening present without a reporter: %s", raw)
	}
}
