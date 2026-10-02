package mcpadapter

import (
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// makeTool returns a placeholder tool with a valid (empty-object) input
// schema. A real upstream's tools/list response always carries one per spec,
// and the official SDK's raw AddTool (see addProxyTools) panics without one,
// so a schema-less test tool would misrepresent what production ever sees.
func makeTool(name string) *mcpsdk.Tool {
	return &mcpsdk.Tool{Name: name, Description: "test tool " + name, InputSchema: map[string]any{"type": "object"}}
}

func TestToolRegistry_RegisterAndLookup(t *testing.T) {
	r := NewToolRegistry()

	tools := []*mcpsdk.Tool{makeTool("hadron_health"), makeTool("hadron_runs_list")}
	mustRegister(t, r, "hadron", nil, tools)

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
	mustRegisterNative(t, r, []*mcpsdk.Tool{makeTool("tether_health")})

	rt, ok := r.Lookup("tether_health")
	if !ok {
		t.Fatal("expected tether_health to be found")
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
	mustRegisterNative(t, r, []*mcpsdk.Tool{makeTool("health")})
	if err := r.Register("upstream-a", nil, []*mcpsdk.Tool{makeTool("health"), makeTool("safe")}); err == nil {
		t.Fatal("collision accepted")
	}
	if _, ok := r.Lookup("upstream-a__health"); ok {
		t.Fatal("silent alias created")
	}
	if _, ok := r.Lookup("safe"); ok {
		t.Fatal("rejected batch partially published")
	}
	if rt, ok := r.Lookup("health"); !ok || rt.ServerID != "" {
		t.Fatal("collision replaced native")
	}
}

func TestToolRegistry_AllDefinitions(t *testing.T) {
	r := NewToolRegistry()
	mustRegisterNative(t, r, []*mcpsdk.Tool{makeTool("tether_z"), makeTool("tether_a")})
	if _, err := r.ReplaceServer("srv", nil, []*mcpsdk.Tool{makeTool("srv_tool")}); err != nil {
		t.Error(err)
	}

	defs := r.AllDefinitions()
	if len(defs) != 3 {
		t.Fatalf("expected 3 definitions, got %d", len(defs))
	}
	// Must be sorted by name.
	if defs[0].Name != "srv_tool" || defs[1].Name != "tether_a" || defs[2].Name != "tether_z" {
		names := make([]string, len(defs))
		for i, d := range defs {
			names[i] = d.Name
		}
		t.Errorf("unexpected order: %v", names)
	}
}

func TestToolRegistry_RemoveServer(t *testing.T) {
	r := NewToolRegistry()
	mustRegister(t, r, "a", nil, []*mcpsdk.Tool{makeTool("a_tool1"), makeTool("a_tool2")})
	mustRegister(t, r, "b", nil, []*mcpsdk.Tool{makeTool("b_tool1")})

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
			if _, err := r.ReplaceServer("srv", nil, []*mcpsdk.Tool{makeTool("srv_tool")}); err != nil {
				t.Error(err)
			}
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = r.AllDefinitions()
	}
	<-done
}

func TestToolRegistry_ReplaceServer(t *testing.T) {
	r := NewToolRegistry()
	mustRegisterNative(t, r, []*mcpsdk.Tool{makeTool("health")})
	mustRegister(t, r, "clockwork", nil, []*mcpsdk.Tool{makeTool("alpha"), makeTool("clockwork_health")})

	delta, err := r.ReplaceServer("clockwork", nil, []*mcpsdk.Tool{
		makeTool("beta"),
		&mcpsdk.Tool{Name: "clockwork_health", Description: "updated health"},
	})

	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup("alpha"); ok {
		t.Fatal("alpha should have been removed")
	}
	if _, ok := r.Lookup("beta"); !ok {
		t.Fatal("beta should have been added")
	}
	if _, ok := r.Lookup("clockwork_health"); !ok {
		t.Fatal("clockwork__health should still exist")
	}
	if len(delta.Added) != 1 || delta.Added[0].Name != "beta" {
		t.Fatalf("unexpected added delta: %+v", delta.Added)
	}
	if len(delta.Updated) != 1 || delta.Updated[0].Name != "clockwork_health" {
		t.Fatalf("unexpected updated delta: %+v", delta.Updated)
	}
	if len(delta.Removed) != 1 || delta.Removed[0] != "alpha" {
		t.Fatalf("unexpected removed delta: %+v", delta.Removed)
	}
}

func mustRegister(t *testing.T, r *ToolRegistry, id string, client upstreamClient, tools []*mcpsdk.Tool) {
	t.Helper()
	if err := r.Register(id, client, tools); err != nil {
		t.Fatal(err)
	}
}
func mustRegisterNative(t *testing.T, r *ToolRegistry, tools []*mcpsdk.Tool) {
	t.Helper()
	if err := r.RegisterNative(tools); err != nil {
		t.Fatal(err)
	}
}
