package workerruntime

import (
	"testing"

	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestSessionAgentURNPrefersTheCatalogURN(t *testing.T) {
	if got := sessionAgentURN(workerwire.LaunchSpec{Actor: "msg://agent/agent-mux/cmd-builder-2", AgentURN: "msg://agent/agent-mux/agt_956miv9zax"}); got != "msg://agent/agent-mux/agt_956miv9zax" {
		t.Fatalf("got %s", got)
	}
	if got := sessionAgentURN(workerwire.LaunchSpec{Actor: "msg://agent/agent-mux/loner"}); got != "msg://agent/agent-mux/loner" {
		t.Fatalf("fallback got %s", got)
	}
}
