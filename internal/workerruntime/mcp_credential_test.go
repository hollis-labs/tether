package workerruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app/sessionrefs"
	"github.com/hollis-labs/tether/internal/workerwire"
)

const testHubMCP = `{"mcpServers":{
 "torque":{"command":"x","args":[]},"tesseract":{"command":"x","args":[]},
 "tether":{"command":"ssh","args":["chrispian@hub","tether","mcp","--daemon-only","--token","static","--scopes","message.write,registry.write,groups.write"]}}}`

func TestPlantHubMCPRefCapturePolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			var spec workerwire.LaunchSpec
			if err := json.Unmarshal([]byte(fmt.Sprintf(`{"session":"worker-session","credential":"test-credential","extract_refs":%t}`, enabled)), &spec); err != nil {
				t.Fatal(err)
			}
			cfg, _, _ := plantedConfig(t, spec)
			args := strings.Join(cfg.Servers["tether"].Args, " ")
			for _, flag := range []string{"--session worker-session", fmt.Sprintf("--extract-refs=%t", enabled)} {
				if !strings.Contains(args, flag) {
					t.Errorf("relay is missing %s", flag)
				}
			}
			var sessionID string
			var extract bool
			planted := cfg.Servers["tether"].Args
			for i, arg := range planted {
				if arg == "--session" && i+1 < len(planted) {
					sessionID = planted[i+1]
				}
				if strings.HasPrefix(arg, "--extract-refs=") {
					extract = arg == "--extract-refs=true"
				}
			}
			sink := &workerRefSink{}
			capture := sessionrefs.New(extract, sessionID, nil, sink, nil)
			capture.Record(context.Background(), sessionrefs.Observation{ToolName: "torque_task_get", Arguments: map[string]any{"id": "CW-20261012-0069"}, Failed: true})
			if len(sink.refs) != 0 {
				t.Fatal("failed tool call captured a ref")
			}
			capture.Record(context.Background(), sessionrefs.Observation{ToolName: "torque_task_get", Arguments: map[string]any{"id": "CW-20261012-0069"}})
			if !enabled {
				if len(sink.refs) != 0 {
					t.Fatal("disabled relay captured refs")
				}
			} else if len(sink.refs) != 1 || sink.refs[0] != (workerRef{"worker-session", "torque_task", "CW-20261012-0069", "read", "proxy"}) {
				t.Fatalf("Record output = %+v", sink.refs)
			}
		})
	}
}

type workerRef struct{ session, kind, ref, relation, source string }
type workerRefSink struct{ refs []workerRef }

func (s *workerRefSink) AttachSessionRef(_ context.Context, session, kind, ref, _ string, relation, source, _ string) error {
	s.refs = append(s.refs, workerRef{session, kind, ref, relation, source})
	return nil
}

func TestPlantHubMCPRefCaptureRejectsUnboundOrUnsafeSession(t *testing.T) {
	for _, spec := range []workerwire.LaunchSpec{
		{Session: "s", ExtractRefs: true},
		{Credential: "test", ExtractRefs: true},
		{Session: "$(touch bad)", Credential: "test", ExtractRefs: true},
	} {
		e := NewExecutor(t.TempDir(), "/opt/tether-worker", nil)
		if err := e.plantHubMCP(t.TempDir(), t.TempDir(), spec); err == nil {
			t.Fatal("unsafe/unbound relay accepted")
		}
	}
}

func TestWorkerExtractRefsRejectsProviderWithoutRelay(t *testing.T) {
	e := NewExecutor(t.TempDir(), "/opt/tether-worker", nil)
	_, err := e.launch(context.Background(), workerwire.LaunchSpec{Session: "s", Provider: "codex", ExtractRefs: true})
	if err == nil || !strings.Contains(err.Error(), "claude MCP relay") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlantHubMCPRefCaptureOverridesStaleTemplate(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	template := strings.Replace(testHubMCP, `"--daemon-only"`, `"--daemon-only","--session","stale","--extract-refs=true"`, 1)
	if err := os.WriteFile(filepath.Join(root, "hub-mcp.json"), []byte(template), 0600); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(root, "/opt/tether-worker", nil)
	if err := e.plantHubMCP(cwd, t.TempDir(), workerwire.LaunchSpec{Session: "current", Credential: "test"}); err != nil {
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
	args := strings.Join(cfg.Servers["tether"].Args, " ")
	if !strings.HasSuffix(args, "--session current --extract-refs=false --token-stdin") {
		t.Fatalf("resolved flags not last: %s", args)
	}
}

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
