//go:build linux

package shimbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/adapters/provider"
	pevents "github.com/hollis-labs/substrate/harness/adapters/provider/events"
	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/llm-core/contracts/runtimes"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/testutil"
)

type fixture struct {
	root, config string
	cfg          bridgeConfig
	host         *exec.Cmd
	child        int
}

func await(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
func fixtureChildPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		return 0, fmt.Errorf("parse fixture child PID: %w", err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("fixture child PID must be positive: %d", pid)
	}
	return pid, nil
}
func startFixture(t *testing.T, journalCap int64) *fixture {
	t.Helper()
	root := testutil.ShortDir(t)
	f := &fixture{root: root, config: filepath.Join(root, "config.json")}
	t.Cleanup(func() {
		if f.host != nil {
			_ = f.host.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- f.host.Wait() }()
			select {
			case e := <-done:
				if e != nil {
					t.Errorf("host shutdown: %v", e)
				}
			case <-time.After(12 * time.Second):
				_ = f.host.Process.Kill()
				<-done
				t.Error("host shutdown timeout")
			}
			t.Logf("cleanup host=%d child=%d", f.host.Process.Pid, f.child)
			if alive(f.host.Process.Pid) {
				t.Errorf("host still alive: %d", f.host.Process.Pid)
			}
		}
		if f.child > 0 && alive(f.child) {
			t.Errorf("child still alive: %d", f.child)
		}
		if e := os.RemoveAll(root); e != nil {
			t.Error(e)
		}
	})
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(filepath.Join(root, "home"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "pin"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	spec := shim.Launch{Session: "urn:session:test", Instance: "urn:instance:test", Generation: 1, Actor: mesh.Actor{URN: "msg://service/test/shim", Kind: mesh.ActorService}, Subject: "urn:session:test", Argv: []string{exe, "-test.run=^TestProviderProcess$"}, Cwd: root, ControlDir: filepath.Join(root, "c"), JournalDir: filepath.Join(root, "j"), Secret: strings.Repeat("s", 32), PinPath: filepath.Join(root, "pin"), PinKey: "test", BootGeneration: "test", Reservation: "test", JournalBytes: journalCap, StopGrace: 50 * time.Millisecond, Heartbeat: time.Second, ClientQueue: 256}
	spec.Env = []string{"SHIM_TEST_PROCESS=child", "SHIM_TEST_CONFIG=" + f.config, "HOME=" + filepath.Join(root, "home"), "TMPDIR=" + root}
	f.cfg = bridgeConfig{Launch: spec, StatePath: filepath.Join(root, "state.json")}
	if e = saveJSON(f.config, f.cfg); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(exe, "-test.run=^TestProviderProcess$")
	cmd.Env = []string{"SHIM_TEST_PROCESS=host", "SHIM_TEST_CONFIG=" + f.config, "HOME=" + filepath.Join(root, "home"), "TMPDIR=" + root}
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	log, e := os.Create(filepath.Join(root, "host.log"))
	if e != nil {
		t.Fatal(e)
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		_ = log.Close()
		t.Fatal(e)
	}
	_ = log.Close()
	f.host = cmd
	await(t, "socket and child pid", func() bool {
		pid, e := fixtureChildPID(filepath.Join(root, "home", "child.pid"))
		if e != nil {
			if errors.Is(e, os.ErrNotExist) {
				return false
			}
			t.Fatalf("child PID witness: %v", e)
		}
		f.child = pid
		_, e = os.Stat(filepath.Join(root, "c", "control.sock"))
		return e == nil
	})
	hostGroup, _ := syscall.Getpgid(cmd.Process.Pid)
	childGroup, _ := syscall.Getpgid(f.child)
	if hostGroup != cmd.Process.Pid || childGroup != f.child || hostGroup == syscall.Getpgrp() {
		t.Fatal("host/child placement not independent")
	}
	t.Logf("detached host pid=%d child pid=%d", cmd.Process.Pid, f.child)
	return f
}

type collector struct {
	mu     sync.Mutex
	raw    bytes.Buffer
	deltas []string
	ids    []string
	done   int
}

func (c *collector) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw.Write(b)
}
func (c *collector) event(e pevents.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch e := e.(type) {
	case pevents.Delta:
		c.deltas = append(c.deltas, e.Text)
	case pevents.Done:
		c.done++
	}
}
func (c *collector) id(s string) { c.mu.Lock(); defer c.mu.Unlock(); c.ids = append(c.ids, s) }
func (c *collector) has(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.deltas {
		if s == text {
			return true
		}
	}
	return false
}
func (c *collector) count() int   { c.mu.Lock(); defer c.mu.Unlock(); return c.done }
func (c *collector) text() string { c.mu.Lock(); defer c.mu.Unlock(); return c.raw.String() }
func (c *collector) idCount() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.ids) }
func startSession(t *testing.T, f *fixture, attach bool, preset string, change ...func(*agentsessions.StartOptions)) (agentsessions.Session, *collector) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	adapter := provider.NewClaudeAdapterStreamingStdio()
	adapter.Binary = exe
	rt, e := agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{ID: "test-claude", Adapter: adapter, Caps: agentsessions.Capabilities{StreamingStdio: true, ProviderSessionID: true}})
	if e != nil {
		t.Fatal(e)
	}
	args := []provider.ArgTemplate{{Kind: provider.ArgLiteral, Value: "-test.run=^TestProviderProcess$"}, {Kind: provider.ArgLiteral, Value: "--"}}
	if attach {
		args = append(args, provider.ArgTemplate{Kind: provider.ArgLiteral, Value: "--attach"})
	}
	template := &agentlaunch.TurnTemplate{Convention: provider.LaunchConvention{Executable: exe, Mode: runtimes.ModeStreamingStdio, Argv: args}}
	c := &collector{}
	opts := agentsessions.StartOptions{Workdir: f.root, LogPath: filepath.Join(f.root, fmt.Sprintf("session-%v.log", attach)), Env: []string{"SHIM_TEST_PROCESS=bridge", "SHIM_TEST_CONFIG=" + f.config, "HOME=" + filepath.Join(f.root, "home"), "TMPDIR=" + f.root}, Launch: template, SessionIDPreset: preset, Fanout: c, OnSessionID: c.id, TypedEventCallback: c.event}
	for _, fn := range change {
		fn(&opts)
	}
	s, e := rt.Start(context.Background(), opts)
	if e != nil {
		t.Fatal(e)
	}
	pid := s.Health().PID
	t.Logf("bridge pid=%d attach=%v preset=%q", pid, attach, preset)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
		_, _ = s.Wait()
		if alive(pid) {
			t.Errorf("bridge still alive: %d", pid)
		}
	})
	return s, c
}
func send(t *testing.T, s agentsessions.Session, text string) {
	t.Helper()
	b, e := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SendInput(context.Background(), b); e != nil {
		t.Fatal(e)
	}
}
func stateAt(t *testing.T, f *fixture, fn func(bridgeState) bool) bridgeState {
	t.Helper()
	var state bridgeState
	defer func() {
		if t.Failed() {
			t.Logf("last persisted bridge state: %+v", state)
		}
	}()
	await(t, "persisted bridge state", func() bool { s, e := readState(f.cfg.StatePath); state = s; return e == nil && fn(s) })
	return state
}
func crashBridge(t *testing.T, s agentsessions.Session) {
	t.Helper()
	pid := s.Health().PID
	if e := syscall.Kill(pid, syscall.SIGKILL); e != nil {
		t.Fatal(e)
	}
	_, e := s.Wait()
	if e == nil {
		t.Fatal("crash should report abnormal exit")
	}
	if alive(pid) {
		t.Fatal("bridge not reaped")
	}
}

func TestClaudeTurnsInterruptCrashAttach(t *testing.T) {
	f := startFixture(t, 16<<20)
	s, c := startSession(t, f, false, "")
	await(t, "init callback", func() bool { return c.idCount() == 1 })
	for _, text := range []string{"one", "two"} {
		n := c.count()
		send(t, s, text)
		await(t, "typed delta", func() bool { return c.has("reply:" + text) })
		await(t, "turn done", func() bool { return c.count() > n })
	}
	send(t, s, "hold")
	await(t, "hold output", func() bool { return c.has("reply:hold") })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e := s.(agentsessions.TurnInterrupter).InterruptTurn(ctx); e != nil {
		t.Fatal(e)
	}
	await(t, "interrupt terminal output", func() bool { return strings.Contains(c.text(), "interrupted") })
	history := snapshot(t, f)
	high := history[len(history)-1].Cursor
	t.Logf("interrupt journal high=%q", high)
	before := stateAt(t, f, func(v bridgeState) bool {
		return interruptCheckpointReady(v, high)
	})
	crashBridge(t, s)
	if !alive(f.child) || !alive(f.host.Process.Pid) {
		t.Fatal("hosted child died with bridge")
	}
	// Inject an unconsumed turn while the controller is absent, then detach.
	client := waitController(t, f)
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"content": "offline"}})
	key := "test-offline"
	raw, _ := json.Marshal(shim.Inject{Key: key, Actor: f.cfg.Launch.Actor, Subject: f.cfg.Launch.Subject, Mode: "input", Delivery: "immediate", Generation: "1", Data: encode(append(b, '\n'))})
	if e := client.SendFrame(shim.Frame{Major: 1, Type: "inject", RequestID: key, Session: f.cfg.Launch.Session, Epoch: client.Epoch, Body: raw}); e != nil {
		t.Fatal(e)
	}
	waitReceipt(t, client, key)
	_ = client.Close()
	s2, c2 := startSession(t, f, true, "fake-native")
	if s2.(interface{ ProviderSessionID() string }).ProviderSessionID() != "fake-native" {
		t.Fatal("preset unavailable immediately")
	}
	await(t, "attach init callback", func() bool { return c2.idCount() == 1 })
	await(t, "offline output replay", func() bool { return c2.has("reply:offline") })
	send(t, s2, "three")
	await(t, "next turn", func() bool { return c2.has("reply:three") })
	stateAt(t, f, func(v bridgeState) bool { return v.Counter > before.Counter })
	joined := c.text() + c2.text()
	for _, text := range []string{"reply:one", "reply:two", "reply:hold", "reply:offline", "reply:three", "done:one", "done:two", "interrupted", "done:offline"} {
		if n := strings.Count(joined, text); n != 1 {
			t.Fatalf("%s occurrences=%d", text, n)
		}
	}
	if strings.Contains(joined, "stderr:") {
		t.Fatal("stderr reached stdout parser")
	}
	t.Log("attach: replay captured init before cursor tail, preset immediate; exactly one of each turn output across crash")
	send(t, s2, "exit")
	code, e := s2.Wait()
	if code != 7 || e == nil {
		t.Fatalf("child exit propagation: %d %v", code, e)
	}
}
func encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func waitReceipt(t *testing.T, c *shim.Client, key string) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-c.Frames:
			if !ok {
				t.Fatal("disconnected")
			}
			if f.ReplyTo == key {
				if f.Type == "error" {
					t.Fatalf("inject: %s", f.Body)
				}
				return
			}
		case <-timer.C:
			t.Fatal("receipt timeout")
		}
	}
}
func TestAttachCarriesPartialLine(t *testing.T) {
	f := startFixture(t, 16<<20)
	s, c := startSession(t, f, false, "")
	await(t, "init", func() bool { return c.idCount() == 1 })
	send(t, s, "partial")
	stateAt(t, f, func(v bridgeState) bool { return bytes.Contains(v.Partial, []byte("split")) })
	crashBridge(t, s)
	if !alive(f.child) {
		t.Fatal("child died")
	}
	s2, c2 := startSession(t, f, true, "fake-native")
	send(t, s2, "finish-partial")
	await(t, "reassembled typed line", func() bool { return c2.has("split-tail") })
	if strings.Count(c.text()+c2.text(), "split-tail") != 1 {
		t.Fatal("split line lost or duplicated")
	}
	t.Log("mid-line cursor resumed using atomically persisted partial stdout")
}
func TestBridgeReportsCapAsJournalUnavailable(t *testing.T) {
	f := startFixture(t, 2<<20)
	s, c := startSession(t, f, false, "")
	await(t, "init", func() bool { return c.idCount() == 1 })
	send(t, s, "burst")
	code, e := s.Wait()
	if code != 93 || e == nil {
		t.Fatalf("bridge failure: %d %v", code, e)
	}
	b, e := os.ReadFile(filepath.Join(f.root, "session-false.log"))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte("journal_unavailable")) {
		t.Fatalf("bridge omitted journal failure: %.300s", b)
	}
	await(t, "child terminated by cap", func() bool { return !alive(f.child) })
	t.Log("2 MiB cap stopped child; upstream gap masks journal_full as journal_unavailable (design mismatch)")
}

func TestUnknownInputOutcomeIsFatalWithoutRetry(t *testing.T) {
	f := startFixture(t, 16<<20)
	s, c := startSession(t, f, false, "")
	await(t, "init", func() bool { return c.idCount() == 1 })
	send(t, s, "block-input")
	await(t, "child no longer reads", func() bool { return c.has("blocked-input") })
	before := stateAt(t, f, func(v bridgeState) bool { return v.Counter == 1 })
	// More than a pipe can hold: the first <=64 KiB inject writes partially,
	// reaches the shim's two-second deadline, and must never be retried.
	sent := make(chan error, 1)
	go func() { sent <- s.SendInput(context.Background(), bytes.Repeat([]byte("x"), 100<<10)) }()
	await(t, "bridge failure", func() bool { return !s.Health().Alive })
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("sender stuck after bridge failure")
	}
	code, e := s.Wait()
	if code != 93 || e == nil {
		t.Fatalf("unknown outcome exit: %d %v", code, e)
	}
	b, e := os.ReadFile(filepath.Join(f.root, "session-false.log"))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(b, []byte("outcome_unknown")) {
		t.Fatalf("unknown not reported: %s", b)
	}
	after, e := readState(f.cfg.StatePath)
	if e != nil {
		t.Fatal(e)
	}
	events := snapshot(t, f)
	unknown := 0
	failedKey := ""
	sawUnknown := false
	for _, ev := range events {
		if ev.Kind == "shim.inject_intent" && sawUnknown {
			t.Fatal("another input sent after outcome_unknown")
		}
		if ev.Kind == "shim.inject_outcome" {
			var p struct {
				Key     string `json:"key"`
				Receipt struct {
					Code  string `json:"code"`
					Bytes int    `json:"bytes"`
				} `json:"receipt"`
			}
			if e := json.Unmarshal(ev.Payload, &p); e != nil {
				t.Fatal(e)
			}
			if p.Receipt.Code == "outcome_unknown" {
				unknown++
				failedKey = p.Key
				sawUnknown = true
			}
		}
	}
	if unknown != 1 || failedKey != "bridge-"+strconv.FormatUint(after.Counter, 10) {
		t.Fatalf("unknown outcomes=%d key=%s counter=%d", unknown, failedKey, after.Counter)
	}
	if !alive(f.child) {
		t.Fatal("bridge failure should detach healthy child")
	}
	t.Logf("outcome_unknown hard failure; persisted counter %d -> %d; no retry", before.Counter, after.Counter)
}

func TestStopDetachesInsteadOfHostedEOF(t *testing.T) {
	f := startFixture(t, 16<<20)
	s, c := startSession(t, f, false, "")
	await(t, "init", func() bool { return c.idCount() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if e := s.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	_, e := s.Wait()
	if e != nil {
		t.Fatal(e)
	}
	if !alive(f.child) {
		t.Fatal("Stop delivered EOF to hosted child unexpectedly")
	}
	t.Log("agentkit Stop closes bridge stdin; bridge detaches immediately; hosted stdin stays open; host must explicitly stop child for user stop")
}

func snapshot(t *testing.T, f *fixture) []mesh.Event {
	t.Helper()
	c, e := shim.Connect(filepath.Join(f.root, "c", "control.sock"), f.cfg.Launch.Secret, f.cfg.Launch.Session, f.cfg.Launch.Instance, "1", "observer", false)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.Replay(f.cfg.Launch.Session, ""); e != nil {
		t.Fatal(e)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	high := ""
	var events []mesh.Event
	for {
		select {
		case frame, ok := <-c.Frames:
			if !ok {
				t.Fatal("snapshot disconnected")
			}
			if frame.Type == "result" {
				var p struct {
					High string `json:"high_water"`
				}
				if e = json.Unmarshal(frame.Body, &p); e != nil {
					t.Fatal(e)
				}
				if p.High != "" {
					high = p.High
				}
			}
			if frame.Type == "event" {
				var p struct {
					Event mesh.Event `json:"event"`
				}
				if e = json.Unmarshal(frame.Body, &p); e != nil {
					t.Fatal(e)
				}
				events = append(events, p.Event)
				if p.Event.Cursor == high {
					return events
				}
			}
		case <-timer.C:
			t.Fatal("snapshot timeout")
		}
	}
}

func TestResourceLimitSeamAffectsBridgeOnly(t *testing.T) {
	f := startFixture(t, 16<<20)
	childLimits, e := os.ReadFile(fmt.Sprintf("/proc/%d/limits", f.child))
	if e != nil {
		t.Fatal(e)
	}
	s, c := startSession(t, f, false, "", func(opts *agentsessions.StartOptions) {
		opts.ResourceLimits = &agentsessions.ResourceLimits{MaxOpenFiles: 256}
	})
	await(t, "wrapped bridge init", func() bool { return c.idCount() == 1 })
	bridgeLimits, e := os.ReadFile(fmt.Sprintf("/proc/%d/limits", s.Health().PID))
	if e != nil {
		t.Fatal(e)
	}
	limitLine := func(b []byte) string {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "Max open files") {
				return line
			}
		}
		return ""
	}
	if !strings.Contains(limitLine(bridgeLimits), "256") {
		t.Fatalf("bridge limit not applied: %s", limitLine(bridgeLimits))
	}
	if limitLine(childLimits) == limitLine(bridgeLimits) {
		t.Fatal("cannot distinguish child and bridge resource limits")
	}
	send(t, s, "wrapped")
	await(t, "wrapped turn", func() bool { return c.has("reply:wrapped") })
	if s.Health().PID == f.child || s.Health().PID == f.host.Process.Pid {
		t.Fatal("Health PID is hosted process")
	}
	t.Logf("Health PID=%d is bridge; host=%d child=%d; bridge limit=%s; child limit=%s", s.Health().PID, f.host.Process.Pid, f.child, limitLine(bridgeLimits), limitLine(childLimits))
}

// Pipe delivery is weaker than arbitrary-crash exactly-once delivery: a pipe write and
// cursor commit are separate effects. Prove the duplicate side of that window.
func TestCrashAfterPipeWriteCanDuplicate(t *testing.T) {
	f := startFixture(t, 16<<20)
	f.cfg.PauseAfterLine = "reply:dup-gate"
	if e := saveJSON(f.config, f.cfg); e != nil {
		t.Fatal(e)
	}
	s, c := startSession(t, f, false, "")
	await(t, "init", func() bool { return c.idCount() == 1 })
	stateAt(t, f, func(v bridgeState) bool { return len(v.Init) > 0 })
	send(t, s, "dup-gate")
	await(t, "first delivery", func() bool { return c.has("reply:dup-gate") })
	await(t, "bridge paused before cursor", func() bool { _, e := os.Stat(filepath.Join(f.root, "after-write")); return e == nil })
	crashBridge(t, s)
	f.cfg.PauseAfterLine = ""
	if e := saveJSON(f.config, f.cfg); e != nil {
		t.Fatal(e)
	}
	_, c2 := startSession(t, f, true, "fake-native")
	await(t, "uncommitted delivery replayed", func() bool { return c2.has("reply:dup-gate") })
	if strings.Count(c.text()+c2.text(), "reply:dup-gate") != 2 {
		t.Fatal("did not expose pipe/commit duplicate window")
	}
	t.Log("expected limitation: crash after consumed pipe write but before durable cursor duplicates the line; cursor cannot deduplicate output already delivered to prior reader")
}

func TestFinalLineWithoutNewlineOnExit(t *testing.T) {
	f := startFixture(t, 16<<20)
	s, c := startSession(t, f, false, "")
	await(t, "init", func() bool { return c.idCount() == 1 })
	send(t, s, "exit-final")
	code, e := s.Wait()
	if code != 7 || e == nil {
		t.Fatalf("final exit: %d %v", code, e)
	}
	if !c.has("final-no-newline") {
		t.Fatal("final EOF-delimited line lost")
	}
	t.Log("last stdout fragment flushed on shim.exit before agentkit reader EOF")
}

func TestAttachRejectsJournalOrIdentityBeforeInput(t *testing.T) {
	f := startFixture(t, 16<<20)
	descriptor := filepath.Join(f.root, "launch.json")
	if e := saveJSON(descriptor, f.cfg.Launch); e != nil {
		t.Fatal(e)
	}
	if e := saveJSON(filepath.Join(f.root, "bridge.json"), Checkpoint{Session: f.cfg.Launch.Session, Instance: f.cfg.Launch.Instance, Generation: 1, Journal: "recorded"}); e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct {
		name, code string
		opts       Options
	}{
		{"journal", "journal_mismatch", Options{DescriptorPath: descriptor, Attach: true, ExpectedJournal: "foreign"}},
		{"identity", "identity_mismatch", Options{DescriptorPath: descriptor, Attach: true, ExpectedSession: "urn:session:foreign", ExpectedJournal: "foreign"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, e := Run(context.Background(), test.opts, io.NopCloser(strings.NewReader("must-not-inject\n")), io.Discard, io.Discard)
			var fault *Failure
			if !errors.As(e, &fault) || fault.Code != test.code {
				t.Fatalf("mismatch: %v", e)
			}
			state, e := ReadCheckpoint(filepath.Join(f.root, "bridge.json"))
			if e != nil && !os.IsNotExist(e) {
				t.Fatal(e)
			}
			if state.Counter != 0 {
				t.Fatal("input injected before mismatch refusal")
			}
		})
	}
}

func TestAttachDiagnosticCountsReplayAndKeepsProviderAlive(t *testing.T) {
	f := startFixture(t, 16<<20)
	descriptor := filepath.Join(f.root, "launch.json")
	if err := saveJSON(descriptor, f.cfg.Launch); err != nil {
		t.Fatal(err)
	}
	observer, err := shim.Connect(filepath.Join(f.cfg.Launch.ControlDir, "control.sock"), f.cfg.Launch.Secret, f.cfg.Launch.Session, f.cfg.Launch.Instance, "1", "observer", false)
	if err != nil {
		t.Fatal(err)
	}
	journal := observer.Journal
	_ = observer.Close()
	if e := saveJSON(filepath.Join(f.root, "bridge.json"), Checkpoint{Session: f.cfg.Launch.Session, Instance: f.cfg.Launch.Instance, Generation: 1, Journal: journal}); e != nil {
		t.Fatal(e)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []AttachEvent
	output := &bytes.Buffer{}
	_, err = Run(ctx, Options{DescriptorPath: descriptor, Attach: true, ExpectedJournal: journal, OnAttach: func(event AttachEvent) error { events = append(events, event); cancel(); return nil }}, reader, output, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("attach catch-up: %v", err)
	}
	if len(events) != 1 || events[0].ReplayedEvents == 0 || events[0].Epoch == "" || events[0].Journal != journal {
		t.Fatalf("attach diagnostic: %+v", events)
	}
	if strings.Count(output.String(), `"subtype":"init"`) != 1 {
		t.Fatalf("bootstrap replay: %q", output.String())
	}
	if !alive(f.child) {
		t.Fatal("attach detach killed provider")
	}
}

// A killed bridge may be reaped before the host observes its closed socket.
func waitController(t *testing.T, f *fixture) *shim.Client {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := shim.Connect(filepath.Join(f.root, "c", "control.sock"), f.cfg.Launch.Secret, f.cfg.Launch.Session, f.cfg.Launch.Instance, "1", "controller", false)
		if err == nil {
			return c
		}
		var fault *shim.Error
		if !errors.As(err, &fault) || fault.Code != "controller_busy" || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestControllerWaitsForRelease(t *testing.T) {
	f := startFixture(t, 16<<20)
	first := waitController(t, f)
	done := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); _ = first.Close(); close(done) }()
	second := waitController(t, f)
	_ = second.Close()
	<-done
}

// A snapshot is a lower bound: later journal events may already be committed.
func interruptCheckpointReady(v bridgeState, high string) bool {
	journal, seq, ok := strings.Cut(v.Cursor, ":")
	snapshotJournal, snapshotSeq, snapshotOK := strings.Cut(high, ":")
	current, currentErr := strconv.ParseUint(seq, 10, 64)
	minimum, minimumErr := strconv.ParseUint(snapshotSeq, 10, 64)
	return ok && snapshotOK && journal != "" && journal == snapshotJournal &&
		currentErr == nil && minimumErr == nil && current >= minimum &&
		v.Counter >= 4 && len(v.Init) > 0 && len(v.Partial) == 0
}

func TestInterruptCheckpointCanAdvancePastSnapshot(t *testing.T) {
	f := startFixture(t, 16<<20)
	s, c := startSession(t, f, false, "")
	await(t, "init callback", func() bool { return c.idCount() == 1 })
	for _, text := range []string{"one", "two"} {
		send(t, s, text)
		await(t, "completed first turns", func() bool { return strings.Contains(c.text(), "done:"+text) })
	}
	history := snapshot(t, f)
	high := history[len(history)-1].Cursor
	for _, text := range []string{"three", "four"} {
		send(t, s, text)
		await(t, "completed later turns", func() bool { return strings.Contains(c.text(), "done:"+text) })
	}
	newer := snapshot(t, f)
	latest := newer[len(newer)-1].Cursor
	current := stateAt(t, f, func(v bridgeState) bool { return interruptCheckpointReady(v, latest) })
	if latest == high {
		t.Fatal("forced progress did not advance journal")
	}
	if !interruptCheckpointReady(current, high) {
		t.Fatalf("committed newer checkpoint rejected: cursor=%q snapshot=%q", current.Cursor, high)
	}
}

func TestInterruptCheckpointRejectsIncompleteOrForeignState(t *testing.T) {
	ready := bridgeState{Cursor: "journal:10", Counter: 4, Init: []byte("init")}
	for _, tc := range []struct {
		name, cursor, high string
		counter            uint64
		init, partial      []byte
	}{
		{"behind", "journal:9", "journal:10", 4, ready.Init, nil},
		{"foreign", "other:10", "journal:10", 4, ready.Init, nil},
		{"malformed", "journal:bad", "journal:10", 4, ready.Init, nil},
		{"missing-init", ready.Cursor, ready.Cursor, 4, nil, nil},
		{"pending-input", ready.Cursor, ready.Cursor, 3, ready.Init, nil},
		{"partial-output", ready.Cursor, ready.Cursor, 4, ready.Init, []byte("partial")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := bridgeState{Cursor: tc.cursor, Counter: tc.counter, Init: tc.init, Partial: tc.partial}
			if interruptCheckpointReady(v, tc.high) {
				t.Fatal("incomplete or foreign checkpoint accepted")
			}
		})
	}
}
