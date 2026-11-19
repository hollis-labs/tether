//go:build linux

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	agentlaunch "github.com/hollis-labs/libs/harness/core/agentlaunch"
	"github.com/hollis-labs/libs/harness/core/runner"
	"github.com/hollis-labs/libs/harness/core/sandbox"
	"github.com/hollis-labs/libs/harness/core/shim"
	"github.com/hollis-labs/libs/llm/core/contracts/runtimes"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatwire"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
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
	turnID := ""
	turns := 0
	autoComplete := os.Getenv("TETHER_CODEX_APP_COMPLETE") == "yes"
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
			turns++
			turnID = "app-turn"
			if turns > 1 {
				turnID = fmt.Sprintf("app-turn-%d", turns)
			}
			result = map[string]any{"turn": map[string]string{"id": turnID}}
		case "turn/interrupt":
			// Finish the surviving turn without repeating its already-accepted start.
			for _, line := range []string{
				fmt.Sprintf(`{"method":"item/agentMessage/delta","params":{"threadId":"app-native","turnId":%q,"itemId":"answer","delta":"fixture recovered tail"}}`, turnID),
				fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"app-native","turn":{"id":%q,"status":"interrupted"}}}`, turnID),
			} {
				if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
					os.Exit(88)
				}
			}
			autoComplete = true
			result = map[string]any{}
		default:
			os.Exit(90)
		}
		if enc.Encode(map[string]any{"id": m.ID, "result": result}) != nil {
			os.Exit(89)
		}
		if m.Method == "turn/start" && autoComplete {
			for _, line := range []string{
				`{"emittedAtMs":1,"method":"turn/started","params":{"threadId":"app-native","turn":{"id":"app-turn","status":"inProgress","items":[],"error":null}}}`,
				`{"method":"item/started","params":{"threadId":"app-native","turnId":"app-turn","startedAtMs":1,"item":{"id":"answer","type":"agentMessage","text":"","phase":"final_answer","delivery":null}}}`,
				`{"method":"item/agentMessage/delta","params":{"threadId":"app-native","turnId":"app-turn","itemId":"answer","delta":"fixture actual reply"}}`,
				`{"method":"item/completed","params":{"threadId":"app-native","turnId":"app-turn","completedAtMs":2,"item":{"id":"answer","type":"agentMessage","text":"fixture actual reply","phase":"final_answer","delivery":null}}}`,
				`{"method":"turn/completed","params":{"threadId":"app-native","turn":{"id":"app-turn","status":"completed","items":[],"error":null}}}`,
			} {
				if _, err := fmt.Fprintln(os.Stdout, strings.ReplaceAll(line, `"app-turn"`, strconv.Quote(turnID))); err != nil {
					os.Exit(88)
				}
			}
		}
	}
	os.Exit(0)
}

func TestHostedCodexActualLaunchAndRecovery(t *testing.T) {
	runHostedCodexDeliveryFixture(t, hostedCodexFixtureOptions{})
}
func TestHostedCodexActualCompletedTurnDeliveryAndRecovery(t *testing.T) {
	runHostedCodexDeliveryFixture(t, hostedCodexFixtureOptions{complete: true})
}
func TestHostedCodexActualRetainedTurnDeliveryAndRecovery(t *testing.T) {
	runHostedCodexDeliveryFixture(t, hostedCodexFixtureOptions{complete: true, retained: true})
}

func TestHostedCodexChatBackend(t *testing.T) {
	runHostedCodexDeliveryFixture(t, hostedCodexFixtureOptions{complete: true, chatBefore: true})
}

func TestHostedCodexChatAfterControllerRecovery(t *testing.T) {
	runHostedCodexDeliveryFixture(t, hostedCodexFixtureOptions{chatAfter: true})
}

type hostedCodexFixtureOptions struct{ complete, retained, chatBefore, chatAfter bool }

func runHostedCodexDeliveryFixture(t *testing.T, options hostedCodexFixtureOptions) {
	complete, retain := options.complete, options.retained
	t.Setenv(EnvLaunchHost, "shim")
	root, err := os.MkdirTemp("/var/tmp", "th2-ca-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("HOME", root)
	t.Setenv("TMPDIR", root)
	// This fixture supplies its workspace directly. Prepare the log parent
	// before output logging is connected on launch and reattachment.
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
	s.shimHosting = &shimHosting{provider: p, instance: "urn:instance:codex-app-fixture", capability: shimhost.Supported, inspect: p.Inspect, prepare: func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
		return shimhost.PrepareProvider(spec, nil, limits)
	}}
	plan := &launch.Plan{ProviderID: "codex", ProviderBrand: "codex", RuntimeKind: "jsonrpc-stdio", Command: exe, RepoRoot: root, WorkRoot: root, BootMode: "none"}
	if complete {
		plan.Route = &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}
	}
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
	if complete {
		opts.Env = append(opts.Env, "TETHER_CODEX_APP_COMPLETE=yes")
	}
	original := &shimcodex.Runtime{Config: shimcodex.Config{ID: "codex"}}
	req, err := s.prepareShimStart(ctx, plan, agentsessions.StartRequest{ID: "codex-app", Runtime: original, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	if req.Runtime == original {
		t.Fatal("actual launch fell back to direct runtime")
	}
	runtime, ok := req.Runtime.(*shimcodex.Runtime)
	if !ok {
		t.Fatal("actual launch bypassed hosted protocol")
	}
	if retain {
		// Reproduce the original durable private-completion boundary: the host
		// and provider run normally, but public delivery is connected at boot.
		runtime.Config.Deliver = nil
	}
	chatEnabled := options.chatBefore
	if chatEnabled || options.chatAfter {
		s.installCodexTap("codex-app", "codex", req.Runtime, &req.Options)
	}
	if err = s.Manager.Start(ctx, req); err != nil {
		t.Fatal(err)
	}
	s.watchSessionBindings("codex-app")
	if chatEnabled {
		be, err := s.ChatBackend("codex-app", chatwire.RuntimeCodexAppServer)
		if err != nil {
			t.Fatal(err)
		}
		chatCtx, stopChat := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- be.Run(chatCtx) }()
		<-be.(interface{ Ready() <-chan struct{} }).Ready()
		run, err := be.Submit(ctx, chatbackend.Turn{Text: "fixture", Context: json.RawMessage(`{"secret":"hosted-private-context"}`)})
		if err != nil {
			stopChat()
			<-done
			t.Fatal(err)
		}
		terminals, deltas := 0, 0
		for ev, err := range run.Events() {
			if err != nil {
				t.Error(err)
			}
			if ev.IsTerminal() {
				terminals++
				if ev.Verb != chatstream.VerbRunFinish {
					t.Errorf("hosted chat terminal: %s", ev.Verb)
				}
			}
			if ev.Verb == chatstream.VerbPartDelta {
				deltas++
			}
		}
		stopChat()
		<-done
		if terminals != 1 || deltas == 0 {
			t.Fatal("hosted stdout did not reach the chat backend")
		}
	} else {
		text := "fixture"
		if options.chatAfter {
			text, err = (chatbackend.Turn{Text: text, Context: json.RawMessage(`{"secret":"recovery-private-context"}`)}).InputText()
			if err != nil {
				t.Fatal(err)
			}
		}
		if err = s.sendTurnJSONRPC(ctx, "codex-app", text); err != nil {
			t.Fatal(err)
		}
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
	if complete {
		deadline := time.Now().Add(5 * time.Second)
		for before.LastTerminal != "app-turn" || !retain && (len(outputEvents(t, s)) == 0 || len(before.Inbox) != 0) {
			if time.Now().After(deadline) {
				t.Fatal("actual provider completion did not earn public durable delivery")
			}
			time.Sleep(10 * time.Millisecond)
			before, err = port.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
		}
		got := outputEvents(t, s)
		if retain {
			if len(got) != 0 || len(before.Inbox) == 0 || before.Delivery != nil {
				t.Fatal("private completion fabricated delivery")
			}
		} else if len(got) != 1 || got[0].MessageID == "" || got[0].ProviderResultID == "" {
			t.Fatal("actual provider output was not staged")
		}
	}
	if err != nil || before.ThreadID != "app-native" || !complete && before.ActiveTurn != "app-turn" || complete && before.LastTerminal != "app-turn" {
		t.Fatalf("launch not durably routed through protocol: %+v %v", before, err)
	}
	if err = s.Manager.Stop(ctx, "codex-app"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Manager.WaitSession(ctx, "codex-app")
	if err = s.waitShimBinding(ctx, "codex-app"); err != nil {
		t.Fatal(err)
	}
	foreign := r
	foreign.Fingerprint += "-foreign"
	if err = s.reattachShim(ctx, shimRow, foreign); shimFailureCode(err) != "identity_mismatch" {
		t.Fatalf("foreign observed receipt accepted before recovery: %v", err)
	}
	detached, err := s.Store.GetSessionContext(ctx, "codex-app")
	if err != nil || detached.State != "detached" {
		t.Fatal("controller loss did not retain a detached session", err)
	}
	s.ReconcileStaleState()
	if _, live := s.Manager.Get("codex-app"); !live {
		t.Fatal("boot recovery did not reattach the exact surviving host")
	}
	after, err := port.Load(ctx)
	if err != nil || after.Epoch <= before.Epoch || after.NextID != before.NextID || after.ThreadID != before.ThreadID || after.ActiveTurn != before.ActiveTurn || after.Exit != nil {
		t.Fatalf("recovery replayed input or lost custody: %+v %v", after, err)
	}
	if len(after.Inbox) != 0 || after.Delivery == nil || after.Delivery.DeliveredHighWater != after.Cursor {
		t.Fatal("recovery claimed readiness without committed drain")
	}
	if complete && len(outputEvents(t, s)) != 1 {
		t.Fatal("controller recovery repeated public output")
	}
	if retain {
		got := outputEvents(t, s)
		if got[0].MessageID == "" || got[0].ProviderResultID == "" {
			t.Fatal("boot recovery failed to stage retained provider output")
		}
	}
	inspection, err := p.Inspect(ctx, r)
	if err != nil || !inspection.Running || inspection.Receipt.ProviderPID != r.ProviderPID {
		t.Fatal("recovery changed provider")
	}
	if options.chatAfter {
		be, err := s.ChatBackend("codex-app", chatwire.RuntimeCodexAppServer)
		if err != nil {
			t.Fatal(err)
		}
		chatCtx, stopChat := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- be.Run(chatCtx) }()
		<-be.(interface{ Ready() <-chan struct{} }).Ready()
		t.Cleanup(func() { stopChat(); <-done })
		if _, err := be.Submit(ctx, chatbackend.Turn{Text: "must not be delivered"}); !errors.Is(err, chatbackend.ErrBusy) {
			t.Fatalf("recovered active turn: %v, want ErrBusy", err)
		}
		busy, err := port.Load(ctx)
		if err != nil || busy.NextID != after.NextID || len(busy.Operations) != len(after.Operations) {
			t.Fatal("busy submission reached the provider", err)
		}
		if _, err = s.Manager.JsonRpcCall(ctx, "codex-app", "turn/interrupt", map[string]string{"threadId": "app-native", "turnId": "app-turn"}); err != nil {
			t.Fatal(err)
		}
		var observed chatbackend.ObservedRun
		select {
		case observed = <-be.Observed():
		case <-ctx.Done():
			t.Fatal("recovered output was not observed", ctx.Err())
		}
		check := func(run chatbackend.Run, want chatstream.Verb, wantText string) {
			t.Helper()
			terminals := 0
			var text strings.Builder
			for ev, err := range run.Events() {
				if err != nil {
					t.Fatal(err)
				}
				if ev.IsTerminal() {
					terminals++
					if ev.Verb != want {
						t.Fatalf("recovered terminal %s, want %s", ev.Verb, want)
					}
				}
				if ev.Verb == chatstream.VerbPartDelta {
					text.WriteString(ev.Text)
				}
				raw, _ := json.Marshal(ev)
				if strings.Contains(string(raw), "recovery-private-context") {
					t.Fatal("private context replayed after recovery")
				}
			}
			if terminals != 1 || !strings.Contains(text.String(), wantText) {
				t.Fatalf("recovered output: %d terminals, text %q", terminals, text.String())
			}
		}
		check(observed.Run, chatstream.VerbRunAbort, "fixture recovered tail")
		if strings.Contains(observed.Text, "recovery-private-context") {
			t.Fatal("private context in observed input")
		}
		next, err := be.Submit(ctx, chatbackend.Turn{Text: "next turn"})
		if err != nil {
			t.Fatal("next recovered turn", err)
		}
		check(next, chatstream.VerbRunFinish, "fixture actual reply")
		final, err := port.Load(ctx)
		if err != nil || final.ThreadID != "app-native" || final.LastTerminal != "app-turn-2" {
			t.Fatal("next turn did not keep native thread", err)
		}
		live, err := p.Inspect(ctx, r)
		if err != nil || !live.Running || live.Receipt.ProviderPID != r.ProviderPID {
			t.Fatal("chat recovery replaced provider", err)
		}
	}
}
