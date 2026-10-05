//go:build linux

package shimcodex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/shimhost"
	"golang.org/x/sys/unix"
)

// FOLLOW-UP: helpers can retain inherited runner lock fds if they outlive a
// killed test. Both roles use parent-death signals; cleanup stops the canonical
// recorded host identity, and the external runner kills/reaps its process group.
func TestCodexProtocolProcess(t *testing.T) {
	role := os.Getenv("TETHER_CODEX_FIXTURE")
	if role == "" {
		return
	}
	parent := os.Getppid()
	if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0) != nil || os.Getppid() != parent {
		os.Exit(96)
	}
	if role == "host" {
		path := ""
		for i, a := range os.Args {
			if a == "--launch" && i+1 < len(os.Args) {
				path = os.Args[i+1]
			}
		}
		spec, err := shim.ReadLaunch(path)
		if err != nil {
			os.Exit(97)
		}
		h, err := shim.Start(spec)
		if err != nil {
			os.Exit(98)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		<-ctx.Done()
		if h.Close() != nil {
			os.Exit(99)
		}
		os.Exit(0)
	}
	if role != "provider" {
		os.Exit(95)
	}
	log, err := os.OpenFile(os.Getenv("TETHER_CODEX_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(94)
	}
	defer log.Close()
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	initialized, notified, thread := false, false, false
	turns := 0
	for scanner.Scan() {
		raw := append([]byte(nil), scanner.Bytes()...)
		if _, err = log.Write(append(raw, '\n')); err != nil {
			os.Exit(93)
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			os.Exit(92)
		}
		var result any
		switch msg.Method {
		case "initialize":
			if initialized {
				os.Exit(91)
			}
			initialized = true
			result = map[string]string{"userAgent": "codex-fixture"}
		case "initialized":
			if !initialized || notified {
				os.Exit(90)
			}
			notified = true
			continue
		case "thread/start":
			if !notified || thread {
				os.Exit(89)
			}
			thread = true
			result = map[string]any{"thread": map[string]string{"id": "native-t"}}
		case "turn/start":
			if !thread {
				os.Exit(88)
			}
			turns++
			id := fmt.Sprintf("turn-%d", turns)
			result = map[string]any{"turn": map[string]string{"id": id}}
			if turns == 1 && os.Getenv("TETHER_CODEX_GATE") != "" {
				if enc.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "native-t", "turn": map[string]string{"id": id}}}) != nil {
					os.Exit(83)
				}
				gate, err := os.Open(os.Getenv("TETHER_CODEX_GATE"))
				if err != nil {
					os.Exit(82)
				}
				one := make([]byte, 1)
				if n, err := gate.Read(one); err != nil || n != 1 {
					os.Exit(81)
				}
				_ = gate.Close()
			}
		case "turn/steer", "turn/interrupt":
			var params struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Expected string `json:"expectedTurnId"`
			}
			if json.Unmarshal(msg.Params, &params) != nil || params.ThreadID != "native-t" {
				os.Exit(79)
			}
			if msg.Method == "turn/steer" {
				if params.Expected != "turn-1" {
					os.Exit(78)
				}
				result = map[string]string{"turnId": "turn-1"}
			} else {
				if params.TurnID != "turn-1" {
					os.Exit(77)
				}
				result = map[string]any{}
			}
		default:
			os.Exit(87)
		}
		if enc.Encode(map[string]any{"id": msg.ID, "result": result}) != nil {
			os.Exit(86)
		}
		if msg.Method == "turn/interrupt" || msg.Method == "turn/start" && os.Getenv("TETHER_CODEX_CONTROLS") == "" {
			status := "completed"
			if msg.Method == "turn/interrupt" {
				status = "interrupted"
			}
			if enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "native-t", "turn": map[string]string{"id": fmt.Sprintf("turn-%d", turns), "status": status}}}) != nil {
				os.Exit(85)
			}
			if os.Getenv("TETHER_CODEX_PARTIAL_EXIT") != "" {
				if _, err := os.Stdout.Write([]byte("{")); err != nil {
					os.Exit(80)
				}
				os.Exit(7)
			}
		}
	}
	if scanner.Err() != nil {
		os.Exit(84)
	}
	os.Exit(0)
}

func TestLibraryShimCodexDetachReattachPreservesProtocol(t *testing.T) {
	libraryProtocolFixture(t, false, false, false)
}

func TestLibraryShimCodexUncertainTurnNeverResubmits(t *testing.T) {
	libraryProtocolFixture(t, true, false, false)
}

func TestLibraryShimCodexAuthenticatedExitRetainsTruncatedOutput(t *testing.T) {
	libraryProtocolFixture(t, false, true, false)
}

func TestLibraryShimCodexSteerInterruptUseObservedTurn(t *testing.T) {
	libraryProtocolFixture(t, false, false, true)
}

func libraryProtocolFixture(t *testing.T, uncertain, terminal, controls bool) {
	// A short private disk path leaves room for the host's Unix socket suffix.
	root, err := os.MkdirTemp("/var/tmp", "th2-cb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "pin")
	if err = os.WriteFile(pin, nil, 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "requests")
	gatePath := ""
	var gate *os.File
	if uncertain {
		gatePath = filepath.Join(root, "release")
		if err = unix.Mkfifo(gatePath, 0600); err != nil {
			t.Fatal(err)
		}
		gate, err = os.OpenFile(gatePath, os.O_RDWR|syscall.O_NONBLOCK, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = gate.Close() })
	}
	p, err := shimhost.New(shimhost.Config{StateDir: filepath.Join(root, "s"), ShimCommand: []string{exe, "-test.run=^TestCodexProtocolProcess$", "--"}, HostEnv: []string{"TETHER_CODEX_FIXTURE=host", "HOME=" + root, "TMPDIR=" + root}, StopGrace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	spec := shim.Launch{Session: "urn:session:codex-fixture", Instance: "urn:instance:fixture", Generation: 1, Actor: mesh.Actor{URN: "msg://service/shim/test", Kind: mesh.ActorService}, Subject: "urn:session:codex-fixture", Argv: []string{exe, "-test.run=^TestCodexProtocolProcess$"}, Env: []string{"TETHER_CODEX_FIXTURE=provider", "TETHER_CODEX_LOG=" + logPath, "TETHER_CODEX_GATE=" + gatePath, "TETHER_CODEX_CONTROLS=" + func() string {
		if controls {
			return "yes"
		}
		return ""
	}(), "TETHER_CODEX_PARTIAL_EXIT=" + func() string {
		if terminal {
			return "yes"
		}
		return ""
	}(), "HOME=" + root, "TMPDIR=" + root}, Cwd: root, PinPath: pin, PinKey: "fixture", BootGeneration: "fixture", Reservation: "fixture", StopGrace: 50 * time.Millisecond, Heartbeat: time.Second}
	var receipt shimhost.Receipt
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if receipt.HostPID > 0 {
			if err := p.Stop(cleanup, receipt); err != nil {
				t.Error(err)
			}
		}
	})
	receipt, err = p.Place(ctx, "fixture-operation", spec)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{}
	config := Config{ID: "fixture", Receipt: receipt, Store: store, Fresh: true, Limits: Limits{InboxItems: 128, InboxBytes: 1 << 20}, Validate: func(ctx context.Context) error { return ctx.Err() }}
	start := func(cfg Config) *Session {
		t.Helper()
		raw, err := (&Runtime{Config: cfg}).Start(ctx, agentsessions.StartOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s := raw.(*Session)
		t.Cleanup(func() { _ = s.Stop(context.Background()); _, _ = s.Wait() })
		return s
	}
	first := start(config)
	if controls {
		if err = first.SendTurn(ctx, "controlled", "", "fixture", "1"); err != nil {
			t.Fatal(err)
		}
		if err = first.Steer(ctx, "foreign-turn", "reject"); !HasCode(err, "turn_mismatch") {
			t.Fatal("foreign turn steered")
		}
		if err = first.Steer(ctx, "turn-1", "steer"); err != nil {
			t.Fatal(err)
		}
		if err = first.InterruptTurn(ctx); err != nil {
			t.Fatal(err)
		}
		final := waitProtocol(ctx, t, first, func(state State) bool { return state.LastTerminal == "turn-1" && state.ActiveTurn == "" })
		if final.Exit != nil {
			t.Fatal("turn interrupt manufactured provider exit")
		}
		inspection, err := p.Inspect(ctx, receipt)
		if err != nil || !inspection.Running {
			t.Fatal("turn interrupt killed provider")
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var request struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &request) != nil {
				t.Fatal("invalid transcript")
			}
			counts[request.Method]++
		}
		if counts["turn/start"] != 1 || counts["turn/steer"] != 1 || counts["turn/interrupt"] != 1 {
			t.Fatalf("control sequence: %v", counts)
		}
		return
	}
	var before State
	if uncertain {
		callCtx, stopCall := context.WithCancel(ctx)
		outcome := make(chan error, 1)
		go func() { outcome <- first.SendTurn(callCtx, "first", "", "fixture", "1") }()
		before = waitProtocol(ctx, t, first, func(s State) bool { return s.ActiveTurn == "turn-1" })
		stopCall()
		select {
		case err = <-outcome:
			if !HasCode(err, "outcome_unknown") {
				t.Fatalf("uncertain outcome: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	} else {
		if err = first.SendTurn(ctx, "first", "", "fixture", "1"); err != nil {
			t.Fatal(err)
		}
		before = waitProtocol(ctx, t, first, func(s State) bool { return s.LastTerminal == "turn-1" })
	}

	if terminal {
		select {
		case <-first.done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		code, err := first.Wait()
		state := first.engine.Snapshot()
		if code != 7 || !HasCode(err, "protocol_truncated") || state.Exit == nil || state.Exit.Status != 7 || string(state.Partial) != "{" || len(state.Inbox) == 0 {
			t.Fatalf("exit/carry erased or fabricated: code=%d err=%v state=%+v", code, err, state)
		}
		return
	}
	_ = first.Stop(ctx)
	_, _ = first.Wait()
	if first.engine.Snapshot().Exit != nil {
		t.Fatal("detach manufactured provider exit")
	}
	inspection, err := p.Inspect(ctx, receipt)
	if err != nil || !inspection.Running || inspection.Receipt.HostPID != receipt.HostPID || inspection.Receipt.ProviderPID != receipt.ProviderPID {
		t.Fatalf("placement changed: %+v %v", inspection, err)
	}
	config.Fresh = false
	second := start(config)
	if uncertain {
		if err = second.SendTurn(ctx, "forbidden replacement", "", "fixture", "1"); !HasCode(err, "turn_active_or_unknown") {
			t.Fatalf("uncertain turn admitted: %v", err)
		}
		if _, err = gate.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		waitProtocol(ctx, t, second, func(s State) bool { return s.LastTerminal == "turn-1" })
	}
	if err = second.SendTurn(ctx, "second", "", "fixture", "1"); err != nil {
		t.Fatal(err)
	}
	after := waitProtocol(ctx, t, second, func(s State) bool { return s.LastTerminal == "turn-2" })
	if after.Epoch <= before.Epoch || after.NextID <= before.NextID || after.ThreadID != before.ThreadID || after.Exit != nil {
		t.Fatalf("identity/epoch changed incorrectly: before=%+v after=%+v", before, after)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	ids := map[string]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(line, &m) != nil {
			t.Fatal("invalid request log")
		}
		counts[m.Method]++
		if len(m.ID) > 0 {
			if ids[string(m.ID)] {
				t.Fatal("request ID reused")
			}
			ids[string(m.ID)] = true
		}
	}
	if counts["initialize"] != 1 || counts["initialized"] != 1 || counts["thread/start"] != 1 || counts["turn/start"] != 2 {
		t.Fatalf("protocol replay: %v", counts)
	}
}
