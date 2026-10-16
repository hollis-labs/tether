package workerruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/workerwire"
)

const testHubMCP = `{"mcpServers":{
 "torque":{"command":"x","args":[]},"tesseract":{"command":"x","args":[]},
 "tether":{"command":"ssh","args":["chrispian@hub","tether","mcp","--daemon-only","--token","static","--scopes","message.write,registry.write,groups.write"]}}}`

func plantedConfig(t *testing.T, spec workerwire.LaunchSpec) (mcpConfig, string, string) {
	t.Helper()
	root, cwd, cred := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "mcp-cred")
	if err := os.WriteFile(filepath.Join(root, "hub-mcp.json"), []byte(testHubMCP), 0600); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(root, "/opt/tether-worker", nil)
	if err := e.plantHubMCP(cwd, cred, spec); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg mcpConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg, string(raw), cred
}

func TestPlantHubMCPKeepsCredentialOutOfConfigArgsAndEnv(t *testing.T) {
	const secret = "tth_super-secret-session-bearer"
	cfg, raw, cred := plantedConfig(t, workerwire.LaunchSpec{Session: "s", Credential: secret, MCPScopes: []string{"message.write", "forums.read"}})
	if strings.Contains(raw, secret) {
		t.Fatal("credential written into .mcp.json")
	}
	entry := cfg.Servers["tether"]
	if entry.Command != "/opt/tether-worker" || len(entry.Env) != 0 {
		t.Fatalf("entry = %+v", entry)
	}
	joined := strings.Join(entry.Args, " ")
	for _, want := range []string{"mcp-relay --token-file " + filepath.Join(cred, "token") + " -- ssh chrispian@hub tether mcp --daemon-only", "--scopes message.write,forums.read", "--token-stdin"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	// This legacy SSH route does not enter the hub-owned /mcp handler.
	// Its API capture is session provenance, not trusted proxy evidence.
	if strings.Contains(joined, "--forward-daemon") {
		t.Fatal("legacy worker unexpectedly entered hub forwarding")
	}
	if strings.Contains(joined, "message.write,registry.write") {
		t.Fatal("static scopes survived")
	}
	info, err := os.Stat(filepath.Join(cred, "token"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token file: %v %v", info, err)
	}
	if b, _ := os.ReadFile(filepath.Join(cred, "token")); strings.TrimSpace(string(b)) != secret {
		t.Fatal("token file content")
	}
	if cfg.Servers["torque"].Command != "x" {
		t.Fatal("other entries must be unchanged")
	}
}

func TestPlantHubMCPWithoutCredentialIsTheLegacySSHEntry(t *testing.T) {
	cfg, _, _ := plantedConfig(t, workerwire.LaunchSpec{Session: "s"})
	if entry := cfg.Servers["tether"]; entry.Command != "ssh" || strings.Contains(strings.Join(entry.Args, " "), "token-stdin") {
		t.Fatalf("legacy entry changed: %+v", entry)
	}
}

func TestRunRelayFeedsCredentialFirstThenStream(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "got")
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "argv") + "\ncat > " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("tth_abc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunRelay(tokenFile, []string{"ssh", "hub", "tether", "mcp"}, strings.NewReader(`{"jsonrpc":"2.0"}`+"\n"), os.Stderr, os.Stderr); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "tth_abc\n{\"jsonrpc\":\"2.0\"}\n" {
		t.Fatalf("stdin = %q", got)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if strings.Contains(string(argv), "tth_abc") {
		t.Fatal("credential reached ssh argv")
	}
	if err := RunRelay(tokenFile, []string{"/bin/sh", "-c", "true"}, strings.NewReader(""), os.Stderr, os.Stderr); err == nil {
		t.Fatal("relay must only run ssh")
	}
}

func TestPlantHubMCPPlantsEditableAgents(t *testing.T) {
	cfg, _, _ := plantedConfig(t, workerwire.LaunchSpec{Session: "s", Credential: "tth_x", MCPScopes: []string{"catalog.write"}, EditableAgents: []string{"chat", "zz-probe"}})
	joined := strings.Join(cfg.Servers["tether"].Args, " ")
	if !strings.Contains(joined, "--editable-agent chat --editable-agent zz-probe") {
		t.Fatalf("allowlist not planted: %s", joined)
	}
	none, _, _ := plantedConfig(t, workerwire.LaunchSpec{Session: "s", Credential: "tth_x"})
	if strings.Contains(strings.Join(none.Servers["tether"].Args, " "), "--editable-agent") {
		t.Fatal("an agent with no editable_agents got an allowlist")
	}
}

func TestPlantHubMCPRefusesEditableAgentsWithoutCredential(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hub-mcp.json"), []byte(testHubMCP), 0600); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(root, "/opt/tether-worker", nil)
	err := e.plantHubMCP(t.TempDir(), filepath.Join(t.TempDir(), "mcp-cred"), workerwire.LaunchSpec{Session: "s", EditableAgents: []string{"chat"}})
	if err == nil || !strings.Contains(err.Error(), "editable_agents") {
		t.Fatalf("err = %v, want refusal rather than a silently unrestricted session", err)
	}
}
