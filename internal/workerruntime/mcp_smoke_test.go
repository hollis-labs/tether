package workerruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSmokeS6(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.json")
	config := mcpConfig{Servers: map[string]mcpEntry{"torque": {Command: "torque-mcp-hub"}, "tesseract": {Command: "ssh", Args: []string{"hub", "tesseract-mcp-stdio", "worker"}}, "tether": {Command: "ssh", Args: []string{"hub", "tether", "mcp", "--daemon-only"}}}}
	b, _ := json.Marshal(config)
	if err := os.WriteFile(source, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ImportHubMCP(root, source); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "boot")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(root, "unused", nil)
	if err := executor.plantMCP(cwd, nil); err != nil {
		t.Fatal(err)
	}
	planted, _ := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if !strings.Contains(string(planted), "--daemon-only") {
		t.Fatal("hub MCP not planted")
	}
	for _, platform := range []string{"linux", "darwin"} {
		definition, err := ServiceDefinition(platform, "/opt/bin/tether-worker", root, "/usr/bin:/bin")
		if err != nil || !strings.Contains(definition, "tether-worker") {
			t.Fatal(platform, err)
		}
	}
	config.Servers["tether"] = mcpEntry{Command: "tether", Args: []string{"mcp"}}
	b, _ = json.Marshal(config)
	_ = os.WriteFile(source, b, 0600)
	if err := ImportHubMCP(root, source); err == nil {
		t.Fatal("local Tether authority accepted")
	}
	t.Log("hub-only MCP planting, local-authority refusal, Linux service and launchd definitions passed")
}

func TestGrantedMCPPlanting(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.json")
	config := mcpConfig{Servers: map[string]mcpEntry{
		"torque":    {Command: "torque-mcp-hub"},
		"tesseract": {Command: "ssh", Args: []string{"hub", "tesseract-mcp-stdio"}},
		"tether":    {Command: "ssh", Args: []string{"hub", "tether", "mcp", "--daemon-only"}},
		"tangent":   {Type: "http", URL: "http://127.0.0.1:7843/mcp"},
		"nanite":    {Command: "nanite-mcp-local"},
	}}
	b, _ := json.Marshal(config)
	if err := os.WriteFile(source, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ImportHubMCP(root, source); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(root, "unused", nil)
	for _, tc := range []struct {
		granted []string
		want    []string
	}{
		{nil, []string{"nanite", "tangent", "tesseract", "tether", "torque"}},
		{[]string{}, []string{"tether"}},
		{[]string{"torque", "tangent", "missing"}, []string{"tangent", "tether", "torque"}},
	} {
		cwd := t.TempDir()
		if err := e.plantMCP(cwd, tc.granted); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
		var got mcpConfig
		_ = json.Unmarshal(b, &got)
		names := []string{}
		for n := range got.Servers {
			names = append(names, n)
		}
		sort.Strings(names)
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("granted %v planted %v want %v", tc.granted, names, tc.want)
		}
	}
	config.Servers["nanite"] = mcpEntry{Command: "tether", Args: []string{"mcp", "--proxy"}}
	b, _ = json.Marshal(config)
	_ = os.WriteFile(source, b, 0600)
	if err := ImportHubMCP(root, source); err == nil {
		t.Fatal("--proxy entry accepted")
	}
}
