package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
)

// A client tells a hub that resolves agent instance addresses from one that parses them
// but wakes the agent's bare session (CW-20261023-0018).
func TestHealthAdvertisesAgentInstanceAddresses(t *testing.T) {
	s := &Server{Config: Config{ListenAddr: "unix:/x"}}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest("GET", "/health", nil))
	var h Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("health body %q: %v", rec.Body.String(), err)
	}
	if !slices.Contains(h.Features, "agent_instance_address") {
		t.Fatalf("features = %v, want agent_instance_address", h.Features)
	}
}
