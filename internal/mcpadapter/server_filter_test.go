package mcpadapter

import (
	"sort"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// flatToolNames applies the identical ServerFilter logic from RunWithProxyOpts
// and returns the tool names that would be registered flat.
func flatToolNames(registry *ToolRegistry, serverFilter []string) []string {
	allowed := make(map[string]struct{}, len(serverFilter))
	for _, id := range serverFilter {
		allowed[id] = struct{}{}
	}
	firehose := len(allowed) == 0

	var names []string
	for _, def := range registry.AllDefinitions() {
		rt, ok := registry.Lookup(def.Name)
		if !ok || rt.ServerID == "" {
			continue
		}
		if !firehose {
			if _, inFilter := allowed[rt.ServerID]; !inFilter {
				continue
			}
		}
		names = append(names, def.Name)
	}
	sort.Strings(names)
	return names
}

// TestServerFilter_Firehose verifies that an empty ServerFilter causes all
// upstream tools (regardless of server) to appear in the flat surface.
func TestServerFilter_Firehose(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{}
	reg.Register("alpha", mc, []mcp.Tool{makeTool("alpha_tool_a"), makeTool("alpha_tool_b")})
	reg.Register("beta", mc, []mcp.Tool{makeTool("beta_tool_x")})
	reg.RegisterNative([]mcp.Tool{makeTool("mux_health")})

	got := flatToolNames(reg, nil)

	want := []string{"alpha_tool_a", "alpha_tool_b", "beta_tool_x"}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("firehose: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("firehose[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestServerFilter_SingleServer verifies that when ServerFilter = ["alpha"],
// only alpha's tools appear flat and beta's tools are excluded.
func TestServerFilter_SingleServer(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{}
	reg.Register("alpha", mc, []mcp.Tool{makeTool("alpha_tool_a"), makeTool("alpha_tool_b")})
	reg.Register("beta", mc, []mcp.Tool{makeTool("beta_tool_x")})
	reg.RegisterNative([]mcp.Tool{makeTool("mux_health")})

	got := flatToolNames(reg, []string{"alpha"})

	want := []string{"alpha_tool_a", "alpha_tool_b"}

	if len(got) != len(want) {
		t.Fatalf("single-server filter: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("single-server filter[%d]: got %q, want %q", i, got[i], want[i])
		}
	}

	// Confirm beta tool is absent.
	for _, name := range got {
		if name == "beta_tool_x" {
			t.Error("beta_tool_x should not appear when ServerFilter=[\"alpha\"]")
		}
	}
}

// TestServerFilter_MultiServer verifies that when ServerFilter = ["alpha","beta"],
// both servers' tools are registered flat while "gamma" is excluded.
func TestServerFilter_MultiServer(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{}
	reg.Register("alpha", mc, []mcp.Tool{makeTool("alpha_tool_a")})
	reg.Register("beta", mc, []mcp.Tool{makeTool("beta_tool_x")})
	reg.Register("gamma", mc, []mcp.Tool{makeTool("gamma_tool_z")})
	reg.RegisterNative([]mcp.Tool{makeTool("mux_health")})

	got := flatToolNames(reg, []string{"alpha", "beta"})

	want := []string{"alpha_tool_a", "beta_tool_x"}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("multi-server filter: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("multi-server filter[%d]: got %q, want %q", i, got[i], want[i])
		}
	}

	// Confirm gamma is absent.
	for _, name := range got {
		if name == "gamma_tool_z" {
			t.Error("gamma_tool_z should not appear when ServerFilter=[\"alpha\",\"beta\"]")
		}
	}
}

// TestServerFilter_UnknownServerInFilter verifies that a nonexistent server ID
// in the filter is silently ignored; only tools from known servers appear.
func TestServerFilter_UnknownServerInFilter(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{}
	reg.Register("alpha", mc, []mcp.Tool{makeTool("alpha_tool_a")})
	reg.RegisterNative([]mcp.Tool{makeTool("mux_health")})

	got := flatToolNames(reg, []string{"alpha", "nonexistent"})

	want := []string{"alpha_tool_a"}

	if len(got) != len(want) {
		t.Fatalf("unknown server in filter: got %v, want %v", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("unknown server in filter[0]: got %q, want %q", got[0], want[0])
	}
}

// TestServerFilter_NativeToolsExcluded verifies that native tools (ServerID == "")
// are always skipped by the flat-registration loop, regardless of filter mode.
func TestServerFilter_NativeToolsExcluded(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{}
	reg.Register("alpha", mc, []mcp.Tool{makeTool("alpha_tool_a")})
	reg.RegisterNative([]mcp.Tool{
		makeTool("mux_health"),
		makeTool("mux_discover"),
		makeTool("mux_call"),
	})

	// Firehose mode — native tools must still be absent from the flat list.
	firehoseGot := flatToolNames(reg, nil)
	for _, name := range firehoseGot {
		switch name {
		case "mux_health", "mux_discover", "mux_call":
			t.Errorf("native tool %q must not appear in flat list (firehose mode)", name)
		}
	}

	// Selective mode — same expectation.
	selectiveGot := flatToolNames(reg, []string{"alpha"})
	for _, name := range selectiveGot {
		switch name {
		case "mux_health", "mux_discover", "mux_call":
			t.Errorf("native tool %q must not appear in flat list (selective mode)", name)
		}
	}
}
