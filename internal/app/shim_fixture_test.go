//go:build linux

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/testutil"
)

func TestShimLaunchProcess(t *testing.T) {
	role := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			role = os.Args[i+1]
			break
		}
	}
	value := func(key string) string {
		for i, arg := range os.Args {
			if arg == key && i+1 < len(os.Args) {
				return os.Args[i+1]
			}
		}
		return ""
	}
	if role != "" {
		// Follow-up: helpers outliving a killed test can hold inherited runner lock
		// fds. Parent-death signals and identity-checked cleanup cover that case.
		// Configure the death signal in each helper, never in production hosting.
		parent := os.Getppid()
		if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0) != nil || os.Getppid() != parent {
			os.Exit(96)
		}
	}
	switch role {
	case "guardian":
		exe, err := os.Executable()
		if err != nil {
			os.Exit(97)
		}
		cmd := exec.Command(exe, "-test.run=^TestShimLaunchProcess$", "--", "provider")
		input, err := cmd.StdinPipe()
		if err != nil {
			os.Exit(98)
		}
		defer input.Close()
		output, err := cmd.StdoutPipe()
		if err != nil || cmd.Start() != nil {
			os.Exit(99)
		}
		reader := bufio.NewScanner(output)
		if !reader.Scan() {
			os.Exit(100)
		}
		fmt.Println(cmd.Process.Pid)
		_ = cmd.Wait()
		os.Exit(0)
	case "host":
		spec, err := shim.ReadLaunch(value("--launch"))
		if err != nil {
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		host, err := shim.Start(spec)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		<-ctx.Done()
		if host.Close() != nil {
			os.Exit(4)
		}
		os.Exit(0)
	case "bridge":
		if gate := value("--handshake-gate"); gate != "" {
			f, err := os.Open(gate)
			if err != nil {
				os.Exit(94)
			}
			_, err = io.ReadFull(f, make([]byte, 1))
			_ = f.Close()
			if err != nil {
				os.Exit(95)
			}
		}
		has := func(key string) bool {
			for _, arg := range os.Args {
				if arg == key {
					return true
				}
			}
			return false
		}
		var heldInput *os.File
		if has("--hold-after-exit") {
			// Run closes its input. Keep a separate descriptor so this seam only
			// releases when the manager closes the pipe's writer explicitly.
			fd, err := unix.Dup(int(os.Stdin.Fd()))
			if err != nil {
				os.Exit(95)
			}
			heldInput = os.NewFile(uintptr(fd), "held-bridge-input")
			defer heldInput.Close()
		}
		code, err := shimbridge.Run(context.Background(), shimbridge.Options{DescriptorPath: value("--descriptor"), Attach: has("--attach"), Takeover: has("--takeover"), ExpectedJournal: value("--journal")}, os.Stdin, os.Stdout, os.Stderr)
		if has("--hold-after-exit") {
			_, _ = io.Copy(io.Discard, heldInput)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(93)
		}
		os.Exit(code)
	case "provider":
		fmt.Println(`{"type":"system","subtype":"init","session_id":"fake-native"}`)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var input struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(scanner.Bytes(), &input) != nil {
				os.Exit(5)
			}
			if input.Message.Content == "partial-exit" {
				fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"unfinished response"}]}}`)
				os.Exit(7)
			}
			if input.Message.Content == "exit" {
				os.Exit(7)
			}
			if input.Message.Content == "probe-state" {
				if _, err := os.ReadFile(filepath.Join(os.Getenv("TEST_SHIM_STATE_DIR"), "launch.json")); err == nil {
					os.Exit(9)
				}
				input.Message.Content = "state-denied"
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "subtype": "success", "uuid": "result-" + input.Message.Content, "session_id": "fake-native", "result": "reply:" + input.Message.Content})
		}
		os.Exit(0)
	}
}

type shimAppFixture struct {
	svc  *Service
	plan *launch.Plan
	req  agentsessions.StartRequest
	root string
}

func shimFixture(t *testing.T) *shimAppFixture {
	t.Helper()
	t.Setenv(EnvLaunchHost, "shim")
	root := testutil.ShortDir(t)
	t.Setenv("HOME", root)
	t.Setenv("TMPDIR", root)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	svc := bindingHarness(t)
	svc.Catalog = &config.Catalog{}
	svc.turnFeeds = map[string]turnFeedRegistration{"claude": {}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := shimhost.New(shimhost.Config{StateDir: filepath.Join(root, "s"), ShimCommand: []string{exe, "-test.run=^TestShimLaunchProcess$", "--", "host"}, HostEnv: trustedShimEnv(), StopGrace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	svc.shimHosting = &shimHosting{provider: p, bridge: []string{exe, "-test.run=^TestShimLaunchProcess$", "--", "bridge"}, instance: "urn:instance:fixture", capability: shimhost.Supported, inspect: p.Inspect,
		prepare: func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
			return shimhost.PrepareProvider(spec, nil, limits)
		}}
	plan := &launch.Plan{LaunchID: "launch", ProjectID: "project", LogicalAgentID: "worker", ProviderID: "claude", ProviderBrand: "claude", RuntimeKind: "streaming-stdio", Command: exe, RepoRoot: root, WorkRoot: root, BootMode: "none"}
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.CreateSession(store.SessionRow{ID: "shim-session", LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID, ProviderID: plan.ProviderID, Workspace: root, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	adapter := gop.NewClaudeAdapterStreamingStdio()
	adapter.Binary = exe
	rt, err := agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{ID: "claude", Kind: "cli", Adapter: adapter, Caps: agentsessions.Capabilities{StreamingStdio: true, ProviderSessionID: true, BinaryRequired: true}})
	if err != nil {
		t.Fatal(err)
	}
	var args []gop.ArgTemplate
	for _, arg := range []string{"-test.run=^TestShimLaunchProcess$", "--", "provider"} {
		args = append(args, gop.ArgTemplate{Kind: gop.ArgLiteral, Value: arg})
	}
	opts := agentsessions.StartOptions{Workdir: root, WorkspaceDir: root, LogPath: filepath.Join(root, "logs", "session.log"), Env: []string{"HOME=" + root, "TMPDIR=" + root, "TEST_SECRET=provider-only"}, BootMode: "none", AttachEnabled: true, Launch: &agentlaunch.TurnTemplate{Convention: gop.LaunchConvention{Executable: exe, Mode: runtimes.ModeStreamingStdio, Argv: args}}}
	f := &shimAppFixture{svc: svc, plan: plan, root: root, req: agentsessions.StartRequest{ID: "shim-session", Runtime: rt, Options: opts}}
	t.Cleanup(func() {
		svc.shimDraining.Store(f.req.ID, true)
		if row, err := svc.Store.SessionShim(context.Background(), f.req.ID); err == nil {
			if receipt, err := loadShimReceipt(row); err == nil && receipt.HostPID > 0 {
				if err = p.Stop(context.Background(), receipt); err != nil {
					t.Errorf("owned host cleanup: %v", err)
					terminateOwnedShimFixture(t, receipt)
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, live := svc.Manager.Get(f.req.ID); live {
			_ = svc.Manager.Stop(ctx, f.req.ID)
			_, _ = svc.Manager.WaitSession(ctx, f.req.ID)
		}
		if err := svc.waitShimBinding(ctx, f.req.ID); err != nil {
			t.Errorf("binding watcher cleanup: %v", err)
		}
	})
	return f
}

func shimAwait(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}

func (f *shimAppFixture) start(t *testing.T) shimhost.Receipt {
	t.Helper()
	req, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil {
		t.Fatal(err)
	}
	if req.Runtime == f.req.Runtime {
		t.Fatal("fixture fell back to direct")
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	output := f.svc.newSessionTurnOutput(*row, f.plan)
	output.wire(req.Runtime, &req.Options)
	f.svc.turnOutputs.Store(f.req.ID, output)
	if err := f.svc.Manager.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	f.svc.leaseActorBinding(f.req.ID, "worker")
	f.svc.watchSessionBindings(f.req.ID)
	var r shimhost.Receipt
	shimAwait(t, "bridge checkpoint", func() bool {
		row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
		if err != nil {
			return false
		}
		r, err = loadShimReceipt(row)
		if err != nil {
			return false
		}
		state, err := shimbridge.ReadCheckpoint(filepath.Join(filepath.Dir(r.DescriptorPath), "bridge.json"))
		return err == nil && len(state.Init) > 0
	})
	return r
}
