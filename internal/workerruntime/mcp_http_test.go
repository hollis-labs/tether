package workerruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPMCPEntryPassesThroughWithoutCommandOrArgs(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.json")
	raw := `{"mcpServers":{
	  "torque":{"command":"torque-mcp-hub"},
	  "tesseract":{"command":"ssh","args":["hub","t"]},
	  "tether":{"command":"ssh","args":["hub","tether","mcp","--daemon-only"]},
	  "tangent":{"type":"http","url":"https://tangent.example/mcp","headers":{"Authorization":"Bearer ${TANGENT_TOKEN}"}}}}`
	if err := os.WriteFile(source, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ImportHubMCP(root, source); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := NewExecutor(root, "unused", nil).plantMCP(cwd, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	var got map[string]map[string]map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	tangent := got["mcpServers"]["tangent"]
	if tangent["type"] != "http" || tangent["url"] != "https://tangent.example/mcp" {
		t.Fatalf("tangent = %v", tangent)
	}
	for _, k := range []string{"command", "args"} {
		if _, ok := tangent[k]; ok {
			t.Fatalf("tangent has %s: %v", k, tangent)
		}
	}
	if h := tangent["headers"].(map[string]any)["Authorization"]; h != "Bearer ${TANGENT_TOKEN}" {
		t.Fatalf("header = %v", h)
	}
	if got["mcpServers"]["torque"]["command"] != "torque-mcp-hub" {
		t.Fatal("stdio entry changed")
	}
}

func TestHTTPMCPInlineHeaderSecretRefused(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.json")
	raw := `{"mcpServers":{"torque":{"command":"x"},"tesseract":{"command":"x"},
	  "tether":{"command":"ssh","args":["--daemon-only"]},
	  "tangent":{"type":"http","url":"https://t/mcp","headers":{"Authorization":"Bearer s3cret"}}}}`
	_ = os.WriteFile(source, []byte(raw), 0600)
	err := ImportHubMCP(root, source)
	if err == nil || !strings.Contains(err.Error(), "environment variable") {
		t.Fatalf("err = %v", err)
	}
}
