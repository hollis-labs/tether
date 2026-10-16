//go:build linux

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	agentlaunch "github.com/hollis-labs/libs/harness/core/agentlaunch"
	"github.com/hollis-labs/libs/harness/core/runner"
	"github.com/hollis-labs/libs/harness/core/sandbox"
	"github.com/hollis-labs/libs/harness/core/shim"
	"github.com/hollis-labs/libs/llm/core/contracts/runtimes"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

// A team delivery to a freshly launched, never-prompted hosted Codex app-server
// session must start a turn: the reply dispatcher is the only thing that ever
// feeds such a session, and nothing has yet happened on it to wake the dispatcher.
// startNeverPromptedHostedCodex launches a hosted Codex app-server fixture and
// leaves it running without ever having taken a turn.
func startNeverPromptedHostedCodex(t *testing.T) (*Service, context.Context) {
	t.Helper()
	t.Setenv(EnvLaunchHost, "shim")
	root, err := os.MkdirTemp("/var/tmp", "th2-ck-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("HOME", root)
	t.Setenv("TMPDIR", root)
	if err = os.MkdirAll(filepath.Join(root, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
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
	s.shimHosting = &shimHosting{provider: p, instance: "urn:instance:codex-kickoff", capability: shimhost.Supported, inspect: p.Inspect, prepare: func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
		return shimhost.PrepareProvider(spec, nil, limits)
	}}
	plan := &launch.Plan{ProviderID: "codex", ProviderBrand: "codex", RuntimeKind: "jsonrpc-stdio", Command: exe, RepoRoot: root, WorkRoot: root, BootMode: "none",
		Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}
	if err = s.Store.CreateSession(store.SessionRow{ID: "codex-kick", ProviderID: "codex", Workspace: root, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), shimFixtureBudget)
	t.Cleanup(cancel)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, live := s.Manager.Get("codex-kick"); live {
			_ = s.Manager.Stop(cleanup, "codex-kick")
			_, _ = s.Manager.WaitSession(cleanup, "codex-kick")
		}
		if row, err := s.Store.SessionShim(cleanup, "codex-kick"); err == nil {
			if r, err := loadShimReceipt(row); err == nil && r.HostPID > 0 {
				_ = p.Stop(cleanup, r)
			}
		}
	})
	opts := agentsessions.StartOptions{Workdir: root, WorkspaceDir: root, Env: []string{"TETHER_CODEX_APP_FIXTURE=yes", "TETHER_CODEX_APP_COMPLETE=yes", "HOME=" + root, "TMPDIR=" + root}, Launch: &agentlaunch.TurnTemplate{Convention: gop.LaunchConvention{Executable: exe, Mode: runtimes.ModeJSONRPCStdio, Argv: []gop.ArgTemplate{{Kind: gop.ArgLiteral, Value: "-test.run=^TestHostedCodexAppProvider$"}}}}}
	req, err := s.prepareShimStart(ctx, plan, agentsessions.StartRequest{ID: "codex-kick", Runtime: &shimcodex.Runtime{Config: shimcodex.Config{ID: "codex"}}, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Manager.Start(ctx, req); err != nil {
		t.Fatal(err)
	}
	s.watchSessionBindings("codex-kick")
	if err = s.Store.UpdateSessionState("codex-kick", "running", 0, nil); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func TestHostedCodexNeverPromptedSessionTakesATeamDelivery(t *testing.T) {
	s, ctx := startNeverPromptedHostedCodex(t)
	d := newReplyDispatcher(ctx, s.Store, s.Registry, s.replyRuntime(), nil)
	s.replies.Store(d)
	t.Cleanup(d.stop)
	from, err := messaging.ParseURN("msg://user/local/operator")
	if err != nil {
		t.Fatal(err)
	}
	reply, _, err := s.Store.CreateRoutingReply(ctx, store.NewRoutingReply{From: from, ParentID: "team-delivery:kick", Body: "hello reviewer", TargetSessionID: "codex-kick", Actor: "msg://user/local/operator", IdempotencyKey: "kick"})
	if err != nil {
		t.Fatal(err)
	}
	d.notifyNew("codex-kick")

	row, err := s.Store.SessionShim(ctx, "codex-kick")
	if err != nil {
		t.Fatal(err)
	}
	port, err := s.Store.CodexProtocolStore(ctx, row.SessionID, row.ShimKey, codexRecordLimit)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, loadErr := port.Load(ctx)
		got, getErr := s.Store.RoutingReply(ctx, reply.ReplyID)
		if loadErr == nil && getErr == nil && state.LastTerminal == "app-turn" && got.State == store.RoutingReplyDelivered {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery never started a turn: reply %+v (%v); protocol %+v (%v)", got, getErr, state, loadErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Priming gives a fresh app-server team member its first turn on its own.
func TestPrimeAppServerSessionTakesAFirstTurn(t *testing.T) {
	s, ctx := startNeverPromptedHostedCodex(t)
	if err := s.primeAppServerSession(ctx, "codex-kick"); err != nil {
		t.Fatal(err)
	}
	row, err := s.Store.SessionShim(ctx, "codex-kick")
	if err != nil {
		t.Fatal(err)
	}
	port, err := s.Store.CodexProtocolStore(ctx, row.SessionID, row.ShimKey, codexRecordLimit)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, loadErr := port.Load(ctx)
		if loadErr == nil && state.ThreadID == "app-native" && state.LastTerminal == "app-turn" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("priming did not run a turn: %+v (%v)", state, loadErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
