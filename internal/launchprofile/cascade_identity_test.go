package launchprofile

import (
	"slices"
	"testing"
)

func TestFoldChainKeepsURNPerAgentButInheritsScopes(t *testing.T) {
	r := &Resolver{}
	parent := &LaunchProfile{ID: "base", URN: "msg://agent/agent-mux/agt_parent", MCPScopes: []string{"message.write"}}
	child := &LaunchProfile{ID: "child", Extends: "base"}
	got := r.foldChain([]*LaunchProfile{parent, child})
	if got.URN != "" {
		t.Fatalf("child inherited parent's URN %q", got.URN)
	}
	if !slices.Equal(got.MCPScopes, []string{"message.write"}) {
		t.Fatalf("scopes = %v", got.MCPScopes)
	}
	own := &LaunchProfile{ID: "own", Extends: "base", URN: "msg://agent/agent-mux/agt_own", MCPScopes: []string{"groups.write"}}
	got = r.foldChain([]*LaunchProfile{parent, own})
	if got.URN != own.URN || !slices.Equal(got.MCPScopes, []string{"groups.write"}) {
		t.Fatalf("own = %q %v", got.URN, got.MCPScopes)
	}
}
