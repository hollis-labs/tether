//go:build linux

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	agentlaunch "github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"golang.org/x/sys/unix"
)

func TestHostedCodexAppProvider(t *testing.T) {
	if os.Getenv("TETHER_CODEX_APP_FIXTURE") == "" {
		return
	}
	// FOLLOW-UP: a killed test's helpers also retain inherited heavytest fds.
	parent := os.Getppid()
	if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0) != nil || os.Getppid() != parent {
		os.Exit(96)
	}
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	initialized, notified, thread := false, false, false
	for scanner.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			os.Exit(95)
		}
		var result any
		switch m.Method {
		case "initialize":
			if initialized {
				os.Exit(94)
			}
			initialized = true
			result = map[string]string{"userAgent": "app-fixture"}
		case "initialized":
			if !initialized || notified {
				os.Exit(93)
			}
			notified = true
			continue
		case "thread/start":
			if !notified || thread {
				os.Exit(92)
			}
			thread = true
			result = map[string]any{"thread": map[string]string{"id": "app-native"}}
		case "turn/start":
			if !thread {
				os.Exit(91)
			}
			result = map[string]any{"turn": map[string]string{"id": "app-turn"}}
		default:
			os.Exit(90)
		}
		if enc.Encode(map[string]any{"id": m.ID, "result": result}) != nil {
			os.Exit(89)
		}
	}
	os.Exit(0)
}

func TestHostedCodexActualLaunchAndRecoveryKeepsOutputPending(t *testing.T) {
	t.Setenv(EnvLaunchHost, "shim")
	root, err := os.MkdirTemp("/var/tmp", "th2-ca-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("HOME", root)
	t.Setenv("TMPDIR", root)
	s := bindingHarness(t)
	s.Catalog = &config.Catalog{}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := shimhost.New(shimhost.Config{StateDir: filepath.Join(root, "s"), ShimCommand: []string{exe, "-test.run=^TestShimLaunchProcess$", "--", "host"}, HostEnv: trustedShimEnv(), StopGrace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s.shimHosting = &shimHosting{provider: p, instance: "urn:instance:codex-app-fixture", capability: shimhost.Supported, inspect: p.Inspect, prepare: func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
		return shimhost.PrepareProvider(spec, nil, limits)
	}}
	plan := &launch.Plan{ProviderID: "codex", ProviderBrand: "codex", RuntimeKind: "jsonrpc-stdio", Command: exe, RepoRoot: root, WorkRoot: root, BootMode: "none"}
	if err = s.Store.CreateSession(store.SessionRow{ID: "codex-app", ProviderID: "codex", Workspace: root, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), shimFixtureBudget)
	defer cancel()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, live := s.Manager.Get("codex-app"); live {
			_ = s.Manager.Stop(cleanup, "codex-app")
			_, _ = s.Manager.WaitSession(cleanup, "codex-app")
		}
		if row, err := s.Store.SessionShim(cleanup, "codex-app"); err == nil {
			if r, err := loadShimReceipt(row); err == nil && r.HostPID > 0 {
				if err = p.Stop(cleanup, r); err != nil {
					t.Error(err)
					terminateOwnedShimFixture(t, r)
				}
			}
		}
	})
	opts := agentsessions.StartOptions{Workdir: root, WorkspaceDir: root, Env: []string{"TETHER_CODEX_APP_FIXTURE=yes", "HOME=" + root, "TMPDIR=" + root}, Launch: &agentlaunch.TurnTemplate{Convention: gop.LaunchConvention{Executable: exe, Mode: runtimes.ModeJSONRPCStdio, Argv: []gop.ArgTemplate{{Kind: gop.ArgLiteral, Value: "-test.run=^TestHostedCodexAppProvider$"}}}}}
	original := &shimcodex.Runtime{Config: shimcodex.Config{ID: "codex"}}
	req, err := s.prepareShimStart(ctx, plan, agentsessions.StartRequest{ID: "codex-app", Runtime: original, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	if req.Runtime == original {
		t.Fatal("actual launch fell back to direct runtime")
	}
	if _, ok := req.Runtime.(*shimcodex.Runtime); !ok {
		t.Fatal("actual launch bypassed hosted protocol")
	}
	if err = s.Manager.Start(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err = s.sendTurnJSONRPC(ctx, "codex-app", "fixture"); err != nil {
		t.Fatal(err)
	}
	shimRow, err := s.Store.SessionShim(ctx, "codex-app")
	if err != nil {
		t.Fatal(err)
	}
	r, err := loadShimReceipt(shimRow)
	if err != nil {
		t.Fatal(err)
	}
	port, err := s.Store.CodexProtocolStore(ctx, shimRow.SessionID, shimRow.ShimKey, codexRecordLimit)
	if err != nil {
		t.Fatal(err)
	}
	before, err := port.Load(ctx)
	if err != nil || before.ThreadID != "app-native" || before.ActiveTurn != "app-turn" || len(before.Inbox) == 0 {
		t.Fatalf("launch not durably routed through protocol: %+v %v", before, err)
	}
	if err = s.Manager.Stop(ctx, "codex-app"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Manager.WaitSession(ctx, "codex-app")
	foreign := r
	foreign.Fingerprint += "-foreign"
	if err = s.reattachShim(ctx, shimRow, foreign); shimFailureCode(err) != "identity_mismatch" {
		t.Fatalf("foreign observed receipt accepted before recovery: %v", err)
	}
	if err = s.reattachShim(ctx, shimRow, r); shimFailureCode(err) != "output_pending" {
		t.Fatalf("reattach not explicitly pending: %v", err)
	}
	after, err := port.Load(ctx)
	if err != nil || after.Epoch <= before.Epoch || after.NextID != before.NextID || after.ThreadID != before.ThreadID || after.ActiveTurn != before.ActiveTurn || after.Exit != nil {
		t.Fatalf("recovery replayed input or lost custody: %+v %v", after, err)
	}
	retained := map[string]shimcodex.Event{}
	for _, ev := range after.Inbox {
		retained[ev.Identity] = ev
	}
	for _, ev := range before.Inbox {
		seen, ok := retained[ev.Identity]
		if !ok || seen.Cursor != ev.Cursor || string(seen.Raw) != string(ev.Raw) {
			t.Fatal("recovery erased or rewrote accepted source event")
		}
		delete(retained, ev.Identity)
	}
	for _, ev := range retained {
		var metadata struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(ev.Raw, &metadata) != nil || metadata.Kind == "" {
			t.Fatal("recovery fabricated provider output")
		}
	}
	inspection, err := p.Inspect(ctx, r)
	if err != nil || !inspection.Running || inspection.Receipt.ProviderPID != r.ProviderPID {
		t.Fatal("pending recovery changed provider")
	}
}
