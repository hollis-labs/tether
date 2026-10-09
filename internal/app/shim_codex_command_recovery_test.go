//go:build linux

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"golang.org/x/sys/unix"
)

func TestHostedCodexCommandProvider(t *testing.T) {
	if os.Getenv("TETHER_CODEX_COMMAND_FIXTURE") != "yes" {
		return
	}
	parent := os.Getppid()
	if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0) != nil || os.Getppid() != parent {
		os.Exit(96)
	}
	enc := json.NewEncoder(os.Stdout)
	scan := bufio.NewScanner(os.Stdin)
	expected := []string{"initialize", "initialized", "thread/start", "turn/start"}
	position := 0
	emit := func(method string, params any) {
		if enc.Encode(map[string]any{"method": method, "params": params}) != nil {
			os.Exit(95)
		}
	}
	command := func(status string) map[string]any {
		item := map[string]any{"id": "command", "type": "commandExecution", "command": "synthetic blocked command", "cwd": "/fixture", "commandActions": []any{}, "source": "agent", "status": status, "aggregatedOutput": nil, "durationMs": nil, "exitCode": nil, "processId": "fixture-process", "pluginId": nil, "scriptPath": nil}
		if status == "completed" {
			item["aggregatedOutput"], item["durationMs"], item["exitCode"] = "private synthetic tool output\n", 1, 0
		}
		return map[string]any{"threadId": "command-native", "turnId": "command-turn", "item": item}
	}
	for scan.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scan.Bytes(), &m) != nil || position >= len(expected) || m.Method != expected[position] {
			os.Exit(94)
		}
		position++
		if m.Method == "initialized" {
			continue
		}
		result := any(map[string]any{})
		if m.Method == "thread/start" {
			result = map[string]any{"thread": map[string]string{"id": "command-native"}}
		}
		if m.Method == "turn/start" {
			result = map[string]any{"turn": map[string]string{"id": "command-turn"}}
		}
		if enc.Encode(map[string]any{"id": m.ID, "result": result}) != nil {
			os.Exit(93)
		}
		if m.Method != "turn/start" {
			continue
		}
		emit("turn/started", map[string]any{"threadId": "command-native", "turn": map[string]string{"id": "command-turn", "status": "inProgress"}})
		for _, method := range []string{"item/started", "item/completed"} {
			// Write native JSON spelling, including HTML characters and spaces,
			// instead of an encoder's already-normalized JSON representation.
			if _, err := fmt.Fprintf(os.Stdout, "{ \"method\":%q, \"params\":{\"threadId\":\"command-native\",\"turnId\":\"command-turn\",\"item\":{\"id\":\"user\",\"type\":\"userMessage\",\"content\":[{\"type\":\"text\",\"text\":\"synthetic > & < input\",\"text_elements\":[]}],\"clientId\":null}} }\n", method); err != nil {
				os.Exit(91)
			}
		}
		emit("item/started", command("inProgress"))
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat("command-release"); err == nil {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(92)
			}
			time.Sleep(10 * time.Millisecond)
		}
		emit("item/commandExecution/outputDelta", map[string]string{"threadId": "command-native", "turnId": "command-turn", "itemId": "command", "delta": "private synthetic tool output\n"})
		emit("item/completed", command("completed"))
		for _, line := range []string{
			`{"method":"item/started","params":{"threadId":"command-native","turnId":"command-turn","item":{"id":"answer","type":"agentMessage","text":"","phase":"final_answer"}}}`,
			`{"method":"item/agentMessage/delta","params":{"threadId":"command-native","turnId":"command-turn","itemId":"answer","delta":"fixture command final"}}`,
			`{"method":"item/completed","params":{"threadId":"command-native","turnId":"command-turn","item":{"id":"answer","type":"agentMessage","text":"fixture command final","phase":"final_answer"}}}`,
			`{"method":"turn/completed","params":{"threadId":"command-native","turn":{"id":"command-turn","status":"completed"}}}`,
		} {
			if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
				os.Exit(91)
			}
		}
		if os.WriteFile("command-emitted", []byte("done"), 0o600) != nil {
			os.Exit(90)
		}
	}
	os.Exit(0)
}

func TestHostedCodexRetainedCommandCompletesThroughBootRecovery(t *testing.T) {
	t.Setenv(EnvLaunchHost, "shim")
	root, err := os.MkdirTemp("/var/tmp", "th2-cc-")
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
	host, err := shimhost.New(shimhost.Config{StateDir: filepath.Join(root, "s"), ShimCommand: []string{exe, "-test.run=^TestShimLaunchProcess$", "--", "host"}, HostEnv: trustedShimEnv(), StopGrace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s.shimHosting = &shimHosting{provider: host, instance: "urn:instance:codex-command", capability: shimhost.Supported, inspect: host.Inspect, prepare: func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
		return shimhost.PrepareProvider(spec, nil, limits)
	}}
	plan := &launch.Plan{ProviderID: "codex", ProviderBrand: "codex", RuntimeKind: "jsonrpc-stdio", Command: exe, RepoRoot: root, WorkRoot: root, BootMode: "none", Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}
	if err = s.Store.CreateSession(store.SessionRow{ID: "codex-command", ProviderID: "codex", Workspace: root, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), shimFixtureBudget)
	defer cancel()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		s.shimDraining.Store("codex-command", true)
		if _, live := s.Manager.Get("codex-command"); live {
			_ = s.Manager.Stop(cleanup, "codex-command")
			_, _ = s.Manager.WaitSession(cleanup, "codex-command")
		}
		if err := s.waitShimBinding(cleanup, "codex-command"); err != nil {
			t.Error(err)
		}
		if row, err := s.Store.SessionShim(cleanup, "codex-command"); err == nil {
			if receipt, err := loadShimReceipt(row); err == nil && receipt.HostPID > 0 {
				if err = host.Stop(cleanup, receipt); err != nil {
					t.Error(err)
					terminateOwnedShimFixture(t, receipt)
				}
			}
		}
	})
	opts := agentsessions.StartOptions{Workdir: root, WorkspaceDir: root, Env: []string{"TETHER_CODEX_COMMAND_FIXTURE=yes", "HOME=" + root, "TMPDIR=" + root}, Launch: &agentlaunch.TurnTemplate{Convention: gop.LaunchConvention{Executable: exe, Mode: runtimes.ModeJSONRPCStdio, Argv: []gop.ArgTemplate{{Kind: gop.ArgLiteral, Value: "-test.run=^TestHostedCodexCommandProvider$"}}}}}
	req, err := s.prepareShimStart(ctx, plan, agentsessions.StartRequest{ID: "codex-command", Runtime: &shimcodex.Runtime{Config: shimcodex.Config{ID: "codex"}}, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the retained unsupported-union boundary without private content
	// from the live case. The provider and host remain ordinary owned fixtures.
	req.Runtime.(*shimcodex.Runtime).Config.Deliver = nil
	if err = s.Manager.Start(ctx, req); err != nil {
		t.Fatal(err)
	}
	s.watchSessionBindings("codex-command")
	if err = s.sendTurnJSONRPC(ctx, "codex-command", "synthetic original turn"); err != nil {
		t.Fatal(err)
	}
	tracked, err := s.Store.SessionShim(ctx, "codex-command")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := loadShimReceipt(tracked)
	if err != nil {
		t.Fatal(err)
	}
	port, err := s.Store.CodexProtocolStore(ctx, "codex-command", tracked.ShimKey, codexRecordLimit)
	if err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "durable original command start", func() bool {
		state, e := port.Load(ctx)
		if e != nil || state.ActiveTurn != "command-turn" {
			return false
		}
		for _, event := range state.Inbox {
			var message struct {
				Method string `json:"method"`
				Params struct {
					Item struct {
						Type string `json:"type"`
					} `json:"item"`
				} `json:"params"`
			}
			if json.Unmarshal(event.Raw, &message) == nil && message.Method == "item/started" && message.Params.Item.Type == "commandExecution" {
				return true
			}
		}
		return false
	})
	before, err := port.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputEvents(t, s)) != 0 {
		t.Fatal("open command fabricated public output")
	}
	if err = s.Manager.Stop(ctx, "codex-command"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Manager.WaitSession(ctx, "codex-command")
	if err = s.waitShimBinding(ctx, "codex-command"); err != nil {
		t.Fatal(err)
	}
	// Emulate an old controller's durable JSON codec only after its reader is
	// gone. No provider/journal bytes are changed. The original offsets remain
	// exact while embedded RawMessage is compacted and HTML-escaped, as in the
	// actual retained user-message failure that preceded command projection.
	legacy, err := port.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type legacyEvent struct {
		Identity string          `json:"identity"`
		Cursor   string          `json:"cursor"`
		Raw      json.RawMessage `json:"raw"`
	}
	legacyInbox := make([]legacyEvent, len(legacy.Inbox))
	for i, event := range legacy.Inbox {
		legacyInbox[i] = legacyEvent{event.Identity, event.Cursor, event.Raw}
	}
	legacyJSON, err := json.Marshal(struct {
		shimcodex.State
		Inbox []legacyEvent `json:"inbox"`
	}{legacy, legacyInbox})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Store.DB().ExecContext(ctx, `UPDATE codex_shim_protocol SET state_json=? WHERE session_id=? AND revision=?`, string(legacyJSON), "codex-command", fmt.Sprint(legacy.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatal("legacy fixture CAS", err)
	}
	legacy, err = port.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shimcodex.BuildDeliveryProjection(legacy); !shimcodex.HasCode(err, "legacy_bytes_pending") {
		t.Fatal("legacy bytes were treated as a source witness", err)
	}
	if err = os.WriteFile(filepath.Join(root, "command-release"), []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "original provider completion while disconnected", func() bool { _, e := os.Stat(filepath.Join(root, "command-emitted")); return e == nil })
	// Only ordinary boot reconciliation attaches the original controller and
	// replays the actual journal after settling the retained command start.
	s.ReconcileStaleState()
	if _, live := s.Manager.Get("codex-command"); !live {
		t.Fatal("retained command prevented exact-host reattach")
	}
	shimAwait(t, "checked public command final", func() bool {
		state, e := port.Load(ctx)
		return e == nil && state.ActiveTurn == "" && state.LastTerminal == "command-turn" && len(state.Inbox) == 0 && len(outputEvents(t, s)) == 1
	})
	after, err := port.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	outputs := outputEvents(t, s)
	if after.Binding != before.Binding || after.NextID != before.NextID || after.ThreadID != before.ThreadID || after.Epoch <= before.Epoch || len(outputs) != 1 || outputs[0].MessageID == "" {
		t.Fatalf("recovery identity/output mismatch: binding=%t next=%d/%d thread=%q/%q epoch=%d/%d outputs=%+v", after.Binding == before.Binding, after.NextID, before.NextID, after.ThreadID, before.ThreadID, after.Epoch, before.Epoch, outputs)
	}
	// Routed events deliberately omit full text. Verify the real selected-route
	// body and native identity in the store, rather than asserting an excerpt.
	tx, err := s.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	output := outputs[0]
	if err = s.Store.VerifyTurnOutputContent(ctx, tx, output.SessionID, output.OutputID, output.MessageID, output.TurnID, output.ProviderResultID, "fixture command final", "final"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	proof, err := s.Store.LoadVerifiedCodexDelivery(ctx, after, after.ReplayHighWater)
	if err != nil || proof.Validate(s.Store, after, after.ReplayHighWater, false) != nil {
		t.Fatal("completion did not earn exact delivery proof", err)
	}
	current, err := loadShimReceipt(tracked)
	if err != nil {
		t.Fatal(err)
	}
	current.Epoch = receipt.Epoch
	if current != receipt {
		t.Fatal("immutable placement changed")
	}
}
