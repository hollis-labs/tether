package workerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/shim"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

const plantHubMCPFile = `{"mcpServers":{
 "torque":{"command":"x","args":[]},"tesseract":{"command":"y","args":[]},"nanite":{"command":"z","args":[]},
 "tether":{"command":"ssh","args":["chrispian@hub","tether","mcp","--daemon-only","--token","static","--scopes","message.write"]}}}`

func httpPlantExecutor(t *testing.T) (*Executor, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hub-mcp.json"), []byte(plantHubMCPFile), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(root, "/opt/tether-worker", nil)
	e.HubMCPURL = "https://tether.hub.example/mcp"
	e.Tokens = &TokenMinter{}
	return e, filepath.Join(t.TempDir(), "ws", "mcp-cred")
}

func plantedTether(t *testing.T, cwd string) (mcpEntry, mcpConfig) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c mcpConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.Servers["tether"], c
}

// CW-20261016-0160 PR B: a launch with a session agent token plants the tether entry as this worker's mcp-http-relay
// against the hub's /mcp, reading the token from the session's refreshed token file. Nothing secret is planted.
func TestPlantHubMCPUsesTheHubHTTPPathWithAnAgentToken(t *testing.T) {
	e, cred := httpPlantExecutor(t)
	cwd := t.TempDir()
	spec := workerwire.LaunchSpec{Session: "s1", Credential: "session-credential-SECRET", MCPServers: []string{"torque"}, MCPPolicy: true,
		AgentTokens: []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}}
	if err := e.plantHubMCP(cwd, cred, spec); err != nil {
		t.Fatal(err)
	}
	tether, config := plantedTether(t, cwd)
	wantToken := filepath.Join(filepath.Dir(cred), "auth", "tether.token")
	want := []string{"mcp-http-relay", "--url", "https://tether.hub.example/mcp", "--token-file", wantToken}
	if tether.Command != "/opt/tether-worker" || strings.Join(tether.Args, " ") != strings.Join(want, " ") || len(tether.Env) != 0 {
		t.Fatalf("tether entry = %+v, want %v", tether, want)
	}
	if _, ok := config.Servers["torque"]; !ok {
		t.Fatal("the granted torque entry was dropped")
	}
	for _, dropped := range []string{"tesseract", "nanite"} {
		if _, ok := config.Servers[dropped]; ok {
			t.Fatalf("ungranted %s was planted", dropped)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(cwd, ".mcp.json"))
	for _, forbidden := range []string{"session-credential-SECRET", `"ssh"`, "--daemon-only", `"--token"`, "--token-stdin"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("planted config contains %q: %s", forbidden, raw)
		}
	}
	// The ssh credential file of the old path is not written for the HTTP path.
	if _, err := os.Stat(filepath.Join(cred, "token")); err == nil {
		t.Fatal("the HTTP path wrote the ssh credential file")
	}
}

// Without everything the HTTP path needs, a launch fails closed unless the degraded ssh fallback is explicitly on,
// and the fallback is logged as degraded.
func TestPlantHubMCPFallsBackToSSHOnlyWhenEnabledAndLogsIt(t *testing.T) {
	agentToken := []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}
	for name, mutate := range map[string]func(*Executor, *workerwire.LaunchSpec){
		"no hub mcp url":       func(e *Executor, _ *workerwire.LaunchSpec) { e.HubMCPURL = "" },
		"worker cannot mint":   func(e *Executor, _ *workerwire.LaunchSpec) { e.Tokens = nil },
		"hub granted no token": func(_ *Executor, s *workerwire.LaunchSpec) { s.AgentTokens = nil },
		"token for another aud": func(_ *Executor, s *workerwire.LaunchSpec) {
			s.AgentTokens = []workerwire.AgentTokenSpec{{Aud: "tesseract", Scopes: []string{"tesseract.read"}}}
		},
		"older hub, no saved policy": func(_ *Executor, s *workerwire.LaunchSpec) { s.MCPPolicy = false },
		"older hub with editable agents": func(_ *Executor, s *workerwire.LaunchSpec) {
			s.MCPPolicy, s.EditableAgents = false, []string{"worker"}
		},
		"plain http, not loopback": func(e *Executor, _ *workerwire.LaunchSpec) { e.HubMCPURL = "http://hub.example.com/mcp" },
	} {
		t.Run(name, func(t *testing.T) {
			e, cred := httpPlantExecutor(t)
			spec := workerwire.LaunchSpec{Session: "s1", Credential: "cred", AgentTokens: agentToken, MCPPolicy: true}
			mutate(e, &spec)

			cwd := t.TempDir()
			err := e.plantHubMCP(cwd, cred, spec)
			if err == nil || !strings.Contains(err.Error(), "mcp_ssh_fallback") {
				t.Fatalf("fallback off: err = %v, want a refusal naming mcp_ssh_fallback", err)
			}
			if _, statErr := os.Stat(filepath.Join(cwd, ".mcp.json")); statErr == nil {
				t.Fatal("a refused launch planted an MCP config")
			}

			e.AllowSSHFallback = true
			var logged bytes.Buffer
			log.SetOutput(&logged)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			cwd = t.TempDir()
			if err := e.plantHubMCP(cwd, cred, spec); err != nil {
				t.Fatalf("fallback on: %v", err)
			}
			tether, _ := plantedTether(t, cwd)
			if len(tether.Args) == 0 || tether.Args[0] != "mcp-relay" {
				t.Fatalf("fallback planted %+v, want the ssh mcp-relay entry", tether)
			}
			if !strings.Contains(logged.String(), "DEGRADED") || strings.Contains(logged.String(), "Credential") {
				t.Fatalf("degraded use not logged cleanly: %q", logged.String())
			}
		})
	}
}

// The whole launch: the outpost mints the session's agent token before the agent starts, the token file exists at
// the path the planted entry names, and the entry runs the HTTP bridge. (The shim host runs in-process.)
func TestLaunchPlantsHTTPRelayAndKeepsTheTokenFileWhereItPoints(t *testing.T) {
	fakeClaudeSleeping(t)
	providerPath, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/token" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(IssuedToken{Token: "aaa.bbb.ccc", ExpiresAt: time.Now().Add(15 * time.Minute), TTL: 900})
	}))
	t.Cleanup(hub.Close)
	root := lingeringTempDir(t)
	// A freshly enrolled worker has no imported SSH template. Its local
	// command declaration also wins over a catalog executable from another host.
	deviceFile := filepath.Join(root, "device.token")
	device, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deviceFile, []byte(device+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sent []workerwire.Frame
	e := NewExecutor(root, "/opt/tether-worker", func(f workerwire.Frame) error { sent = append(sent, f); return nil })
	e.Config.ProviderCommands = map[string]string{"claude": providerPath}
	e.Config.WorkspaceRoot = t.TempDir()
	e.HubMCPURL = "https://tether.hub.example/mcp"
	e.Tokens = &TokenMinter{Hub: hub.URL, DeviceTokenFile: deviceFile}
	var hosts []*shim.Host
	e.Spawn = func(descriptor string) (*exec.Cmd, error) {
		spec, err := shim.ReadLaunch(descriptor)
		if err != nil {
			return nil, err
		}
		h, err := shim.Start(spec)
		if err == nil {
			hosts = append(hosts, h)
		}
		return nil, err
	}
	t.Cleanup(func() {
		for _, h := range hosts {
			_ = h.Close()
		}
	})
	spec := workerwire.LaunchSpec{Session: "s-http", MCPPolicy: true, Actor: "msg://agent/agent-mux/agt_http_probe", Provider: "claude", Command: "missing-on-this-worker", RuntimeKind: "streaming-stdio",
		Credential: "cred", MCPServers: []string{}, AgentTokens: []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}}
	raw, _ := json.Marshal(spec)
	if err := e.Command(context.Background(), workerwire.Frame{T: "launch", CmdID: "c1", Session: "s-http", Spec: raw}); err != nil {
		t.Fatal(err)
	}
	var workspace string
	for _, f := range sent {
		if f.T == "launched" {
			workspace = f.Workspace
		}
	}
	if workspace == "" {
		t.Fatalf("no launched frame: %+v", sent)
	}
	tether, _ := plantedTether(t, workspace)
	if len(tether.Args) != 5 || tether.Args[0] != "mcp-http-relay" {
		t.Fatalf("planted tether entry = %+v", tether)
	}
	tokenPath := tether.Args[4]
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the planted token file %s: %v %v", tokenPath, info, err)
	}
	if b, _ := os.ReadFile(tokenPath); strings.TrimSpace(string(b)) != "aaa.bbb.ccc" {
		t.Fatalf("token file holds %q", b)
	}
}

// An agent with editable_agents uses /mcp when the hub saved the policy (which carries the allowlist and which the
// hub enforces); the worker no longer needs to refuse it. The allowlist is not planted: the hub applies it.
func TestPlantHubMCPAllowsEditableAgentsWhenTheHubSavedThePolicy(t *testing.T) {
	e, cred := httpPlantExecutor(t)
	cwd := t.TempDir()
	spec := workerwire.LaunchSpec{Session: "s1", Credential: "cred", EditableAgents: []string{"chat"}, MCPPolicy: true,
		AgentTokens: []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}}
	if err := e.plantHubMCP(cwd, cred, spec); err != nil {
		t.Fatalf("editable_agents with a saved policy: %v", err)
	}
	tether, _ := plantedTether(t, cwd)
	if len(tether.Args) == 0 || tether.Args[0] != "mcp-http-relay" || strings.Contains(strings.Join(tether.Args, " "), "editable") {
		t.Fatalf("planted %+v", tether)
	}
	// Without the saved policy (an older hub) it is refused, and only the explicit fallback runs.
	spec.MCPPolicy = false
	if err := e.plantHubMCP(t.TempDir(), cred, spec); err == nil || !strings.Contains(err.Error(), "older hub") {
		t.Fatalf("older hub: err = %v", err)
	}
}
