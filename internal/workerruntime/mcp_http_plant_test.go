package workerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tether/internal/runtoken"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
	for _, torque := range []string{"", "42"} {
		t.Run("torque-"+torque, func(t *testing.T) { testLaunchRunTokenPrivateDescriptor(t, torque) })
	}
}

func testLaunchRunTokenPrivateDescriptor(t *testing.T, torque string, delayed ...bool) {
	delay := len(delayed) > 0 && delayed[0]
	gate := filepath.Join(t.TempDir(), "start")
	var delayedDescriptor string
	var delayedChild *exec.Cmd
	runSecret := ""
	if runtoken.HandoffSupported() {
		runSecret = "synthetic-worker-run-token-never-persist"
	}
	for _, k := range []string{"TETHER_TASK_ID", "TETHER_WORKSTREAM_ID", "TETHER_PROJECT_ID", "TETHER_RUN_ID", runtoken.IDEnv, runtoken.TokenEnv} {
		t.Setenv(k, "ambient-wrong")
	}

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
		if delay {
			delayedDescriptor = descriptor
			executable, err := os.Executable()
			if err != nil {
				return nil, err
			}
			cmd := exec.Command(executable, "-test.run=^TestDelayedWorkerRunShimProcess$")
			cmd.Env = append(os.Environ(), "TETHER_TEST_DELAYED_DESCRIPTOR="+descriptor, "TETHER_TEST_DELAYED_GATE="+gate)
			delayedChild = cmd
			return cmd, cmd.Start()
		}
		spec, err := shim.ReadLaunch(descriptor)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(descriptor)
		if err != nil {
			return nil, err
		}
		if runSecret != "" && strings.Contains(string(data), runSecret) {
			t.Fatal("run token persisted in durable worker descriptor")
		}
		env := strings.Join(spec.Env, "\n")
		if !strings.Contains(env, "TETHER_TASK_ID=CW-20261012-0062") || !strings.Contains(env, "TETHER_PROJECT_ID=PRJ-1") || strings.Contains(env, "TETHER_WORKSTREAM_ID=") || strings.Contains(env, "ambient-wrong") {
			t.Fatalf("incorrect launched correlation environment")
		}
		spec.Env, err = runtoken.RestoreEnvironment(spec.Env, spec.Session)
		if err != nil {
			return nil, err
		}
		if valueInEnv(spec.Env, runtoken.TokenEnv) != runSecret || valueInEnv(spec.Env, runtoken.IDEnv) != "worker-execution" || valueInEnv(spec.Env, "TETHER_RUN_ID") != torque {
			t.Fatal("runtime handoff or Torque/execution source lost")
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
	spec := workerwire.LaunchSpec{Env: map[string]string{"TETHER_TASK_ID": "CW-20261012-0062", "TETHER_PROJECT_ID": "PRJ-1"}, Session: "s-http", MCPPolicy: true, Actor: "msg://agent/agent-mux/agt_http_probe", Provider: "claude", Command: "missing-on-this-worker", RuntimeKind: "streaming-stdio",
		Credential: "cred", MCPServers: []string{}, AgentTokens: []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}}
	if runSecret != "" {
		spec.Env[runtoken.TokenEnv] = runSecret
	}
	spec.Env[runtoken.IDEnv] = "worker-execution"
	if torque != "" {
		spec.Env["TETHER_RUN_ID"] = torque
	}
	raw, _ := json.Marshal(spec)
	launchCtx := context.Background()
	if delay {
		var cancel context.CancelFunc
		launchCtx, cancel = context.WithTimeout(launchCtx, 150*time.Millisecond)
		defer cancel()
	}
	launchErr := e.Command(launchCtx, workerwire.Frame{T: "launch", CmdID: "c1", Session: "s-http", Spec: raw})
	if delay {
		if launchErr == nil {
			t.Fatal("delayed worker unexpectedly ready")
		}
		t.Cleanup(func() {
			if delayedChild != nil && delayedChild.Process != nil {
				_ = delayedChild.Process.Signal(syscall.SIGTERM)
			}
		})
		if len(delayed) > 1 && delayed[1] {
			if err := delayedChild.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			descriptor, err := shim.ReadLaunch(delayedDescriptor)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := runtoken.RestoreEnvironment(descriptor.Env, descriptor.Session)
				if errors.Is(err, runtoken.ErrHandoff) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("terminated worker retained handoff")
				}
				time.Sleep(20 * time.Millisecond)
			}
			return
		}
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(gate + ".ready"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("cancellation revoked submitted worker token")
			}
			time.Sleep(10 * time.Millisecond)
		}
		descriptor, err := shim.ReadLaunch(delayedDescriptor)
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, err := runtoken.RestoreEnvironment(descriptor.Env, descriptor.Session)
			if errors.Is(err, runtoken.ErrHandoff) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("ready worker retained handoff")
			}
			time.Sleep(20 * time.Millisecond)
		}
		return
	}
	if err := launchErr; err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Type().IsRegular() {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			if runSecret != "" && strings.Contains(string(b), runSecret) {
				t.Fatal("run token persisted in worker state")
			}
		}
		return nil
	}); err != nil {
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

func valueInEnv(env []string, key string) string {
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		if k == key {
			return v
		}
	}
	return ""
}

func TestWorkerRunTokenSurvivesCallerCancellation(t *testing.T) {
	if !runtoken.HandoffSupported() {
		t.Skip("requires nonpersistent handoff")
	}
	testLaunchRunTokenPrivateDescriptor(t, "", true)
}
func TestDelayedWorkerRunShimProcess(t *testing.T) {
	path := os.Getenv("TETHER_TEST_DELAYED_DESCRIPTOR")
	if path == "" {
		return
	}
	gate := os.Getenv("TETHER_TEST_DELAYED_GATE")
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	spec, err := shim.ReadLaunch(path)
	if err != nil {
		os.Exit(21)
	}
	spec.Env, err = runtoken.RestoreEnvironment(spec.Env, spec.Session)
	if err != nil {
		os.Exit(22)
	}
	if valueInEnv(spec.Env, runtoken.TokenEnv) != "synthetic-worker-run-token-never-persist" {
		os.Exit(23)
	}
	host, err := shim.Start(spec)
	if err != nil {
		os.Exit(24)
	}
	defer host.Close()
	if err := os.WriteFile(gate+".ready", []byte("ready"), 0600); err != nil {
		os.Exit(25)
	}
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM)
	defer signal.Stop(done)
	<-done
}

func TestWorkerRunTokenReleasedAfterVerifiedExit(t *testing.T) {
	if !runtoken.HandoffSupported() {
		t.Skip("requires nonpersistent handoff")
	}
	testLaunchRunTokenPrivateDescriptor(t, "", true, true)
}
