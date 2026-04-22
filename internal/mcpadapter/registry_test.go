package mcpadapter

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func makeTool(name string) mcp.Tool {
	return mcp.NewTool(name, mcp.WithDescription("test tool "+name))
}

func TestToolRegistry_RegisterAndLookup(t *testing.T) {
	r := NewToolRegistry()

	tools := []mcp.Tool{makeTool("hadron_health"), makeTool("hadron_runs_list")}
	r.Register("hadron", nil, tools)

	rt, ok := r.Lookup("hadron_health")
	if !ok {
		t.Fatal("expected hadron_health to be found")
	}
	if rt.ServerID != "hadron" {
		t.Errorf("ServerID = %q, want %q", rt.ServerID, "hadron")
	}
}

func TestToolRegistry_RegisterNative(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterNative([]mcp.Tool{makeTool("mux_health")})

	rt, ok := r.Lookup("mux_health")
	if !ok {
		t.Fatal("expected mux_health to be found")
	}
	if rt.ServerID != "" {
		t.Errorf("native tool should have empty ServerID, got %q", rt.ServerID)
	}
	if rt.Client != nil {
		t.Error("native tool should have nil Client")
	}
}

func TestToolRegistry_Collision(t *testing.T) {
	r := NewToolRegistry()

	// Register a native "health" tool first.
	r.RegisterNative([]mcp.Tool{makeTool("health")})

	// Register an upstream server that also has "health".
	r.Register("upstream-a", nil, []mcp.Tool{makeTool("health")})

	// Original "health" must still exist and belong to native (empty serverID).
	orig, ok := r.Lookup("health")
	if !ok {
		t.Fatal("original 'health' tool should still exist")
	}
	if orig.ServerID != "" {
		t.Errorf("original health serverID = %q, want empty (native)", orig.ServerID)
	}

	// Disambiguated copy must exist under "upstream-a__health".
	disambig, ok := r.Lookup("upstream-a__health")
	if !ok {
		t.Fatal("disambiguated tool 'upstream-a__health' should exist")
	}
	if disambig.ServerID != "upstream-a" {
		t.Errorf("disambig serverID = %q, want %q", disambig.ServerID, "upstream-a")
	}
}

func TestToolRegistry_AllDefinitions(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterNative([]mcp.Tool{makeTool("mux_z"), makeTool("mux_a")})
	r.Register("srv", nil, []mcp.Tool{makeTool("srv_tool")})

	defs := r.AllDefinitions()
	if len(defs) != 3 {
		t.Fatalf("expected 3 definitions, got %d", len(defs))
	}
	// Must be sorted by name.
	if defs[0].Name != "mux_a" || defs[1].Name != "mux_z" || defs[2].Name != "srv_tool" {
		names := make([]string, len(defs))
		for i, d := range defs {
			names[i] = d.Name
		}
		t.Errorf("unexpected order: %v", names)
	}
}

func TestToolRegistry_RemoveServer(t *testing.T) {
	r := NewToolRegistry()
	r.Register("a", nil, []mcp.Tool{makeTool("a_tool1"), makeTool("a_tool2")})
	r.Register("b", nil, []mcp.Tool{makeTool("b_tool1")})

	r.RemoveServer("a")

	if _, ok := r.Lookup("a_tool1"); ok {
		t.Error("a_tool1 should have been removed")
	}
	if _, ok := r.Lookup("a_tool2"); ok {
		t.Error("a_tool2 should have been removed")
	}
	if _, ok := r.Lookup("b_tool1"); !ok {
		t.Error("b_tool1 should still exist")
	}
}

func TestToolRegistry_ConcurrentAccess(t *testing.T) {
	r := NewToolRegistry()
	done := make(chan struct{})

	go func() {
		for i := 0; i < 100; i++ {
			r.Register("srv", nil, []mcp.Tool{makeTool("srv_tool")})
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = r.AllDefinitions()
	}
	<-done
}
