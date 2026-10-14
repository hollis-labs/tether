package workerruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHubMCPFileRejectsProxy(t *testing.T) {
	root := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(root, "hub-mcp.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"mcpServers":{"tether":{"command":"ssh","args":["hub","tether","mcp","--proxy","--servers","tangent"]}}}`)
	if err := ValidateHubMCPFile(root); err == nil || !strings.Contains(err.Error(), "--proxy") {
		t.Fatalf("want proxy rejection, got %v", err)
	}
	write(`{"mcpServers":{"tether":{"command":"ssh","args":["hub","tether","mcp","--daemon-only"]}}}`)
	if err := ValidateHubMCPFile(root); err != nil {
		t.Fatal(err)
	}
	write(`{"mcpServers":{}}`)
	if err := ValidateHubMCPFile(root); err != nil {
		t.Fatal(err)
	}
}
