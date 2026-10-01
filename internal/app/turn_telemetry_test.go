package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/agentkit/agentsessions"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runner/runner"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

// turnEventRecorder records published events, safe for the reader goroutine.
type turnEventRecorder struct {
	mu  sync.Mutex
	evs []events.Event
}

func (c *turnEventRecorder) Publish(_ context.Context, e events.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, e)
	return nil
}

func (c *turnEventRecorder) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.evs))
	for i, e := range c.evs {
		out[i] = e.Kind
	}
	return out
}

// payload decodes the n-th event of kind into a map.
func (c *turnEventRecorder) payload(t *testing.T, kind string, n int) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := 0
	for _, e := range c.evs {
		if e.Kind != kind {
			continue
		}
		if seen == n {
			var m map[string]any
			if err := json.Unmarshal([]byte(e.PayloadJSON), &m); err != nil {
				t.Fatalf("payload of %s: %v", kind, err)
			}
			if e.SessionID != "s1" || e.LogicalAgentID != "agent" || e.Scope != events.ScopeSession {
				t.Fatalf("%s envelope = %+v; want session-scoped s1/agent", kind, e)
			}
			return m
		}
		seen++
	}
	t.Fatalf("no %s #%d among %v", kind, n, c.kinds())
	return nil
}

func newTestTelemetry() (*turnTelemetry, *turnEventRecorder) {
	pub := &turnEventRecorder{}
	return newTurnTelemetry(pub, "s1", "agent", "opencode-cli", "anthropic/claude-sonnet-4-5"), pub
}

// OpenCode reports usage per step: the turn's usage is their sum (cost
// included), published once at the turn's end, and the reply is each named
// block's latest text.
func TestTurnTelemetry_SumsPerStepUsageAndPublishesOnce(t *testing.T) {
	tel, pub := newTestTelemetry()
	for _, ev := range []gopevents.Event{
		gopevents.SessionID{ID: "ses_1"},
		gopevents.Delta{Text: "Look", BlockID: "p1"},
		gopevents.Delta{Text: "Looking at it", BlockID: "p1"},
		gopevents.ToolUse{ID: "t1", Name: "read"},
		gopevents.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50, CostUSD: 0.01, StopReason: "tool_use"},
		gopevents.Delta{Text: "Done.", BlockID: "p2"},
		gopevents.Usage{InputTokens: 120, OutputTokens: 5, CacheCreationTokens: 7, CostUSD: 0.002, StopReason: "end_turn"},
		gopevents.Done{StopReason: "end_turn"},
	} {
		tel.observe(ev)
	}
	if got := pub.kinds(); strings.Join(got, ",") != "session.turn_output,provider.turn_usage" {
		t.Fatalf("published %v; want one turn_output then one turn_usage", got)
	}
	u := pub.payload(t, events.KindProviderTurnUsage, 0)
	want := map[string]any{
		"session_id": "s1", "provider": "opencode-cli", "model": "anthropic/claude-sonnet-4-5",
		"input_tokens": 220.0, "output_tokens": 15.0, "cache_creation_tokens": 7.0, "cache_read_tokens": 50.0,
		"stop_reason": "end_turn",
	}
	for k, v := range want {
		if u[k] != v {
			t.Errorf("turn_usage[%s] = %v, want %v", k, u[k], v)
		}
	}
	if c, _ := u["cost_usd"].(float64); c < 0.0119 || c > 0.0121 {
		t.Errorf("turn_usage cost_usd = %v, want 0.012", u["cost_usd"])
	}
	out := pub.payload(t, events.KindSessionTurnOutput, 0)
	if out["text"] != "Looking at it\nDone." || out["truncated"] != false || out["text_bytes"] != float64(len("Looking at it\nDone.")) || out["stop_reason"] != "end_turn" {
		t.Fatalf("turn_output = %v", out)
	}

	// The next turn starts from zero.
	tel.observe(gopevents.Usage{InputTokens: 1, OutputTokens: 1})
	tel.observe(gopevents.Done{})
	if u := pub.payload(t, events.KindProviderTurnUsage, 1); u["input_tokens"] != 1.0 {
		t.Fatalf("second turn's usage = %v, want only its own", u)
	}
}

// Codex sends its narration and then the final message: the reply is the
// final text only. Claude's narration blocks, with no final one, all count.
func TestTurnTelemetry_ReplyText(t *testing.T) {
	tel, pub := newTestTelemetry()
	tel.observe(gopevents.Delta{Text: "thinking out loud", Phase: "narration"})
	tel.observe(gopevents.Delta{Text: "CODEXOK", Phase: "final", BlockID: "item_1"})
	tel.observe(gopevents.Done{})
	if out := pub.payload(t, events.KindSessionTurnOutput, 0); out["text"] != "CODEXOK" {
		t.Fatalf("codex reply = %q, want only the final message", out["text"])
	}

	tel.observe(gopevents.Delta{Text: "First.", Phase: "narration", BlockID: "m1:0"})
	tel.observe(gopevents.Delta{Text: "Second.", Phase: "narration", BlockID: "m2:0"})
	tel.observe(gopevents.Done{})
	if out := pub.payload(t, events.KindSessionTurnOutput, 1); out["text"] != "First.\nSecond." {
		t.Fatalf("claude reply = %q", out["text"])
	}

	// Streaming chunks with no block id append.
	tel.observe(gopevents.Delta{Text: "Hel"})
	tel.observe(gopevents.Delta{Text: "lo"})
	tel.observe(gopevents.Done{})
	if out := pub.payload(t, events.KindSessionTurnOutput, 2); out["text"] != "Hello" {
		t.Fatalf("chunked reply = %q", out["text"])
	}

	// A Done with nothing before it publishes nothing.
	n := len(pub.kinds())
	tel.observe(gopevents.Done{})
	if len(pub.kinds()) != n {
		t.Fatalf("an empty turn published %v", pub.kinds()[n:])
	}
}

// A long reply is capped at 4 KB on a rune boundary, with the full length
// and a truncated flag.
func TestTurnTelemetry_CapsLongText(t *testing.T) {
	tel, pub := newTestTelemetry()
	long := strings.Repeat("é", 3000) // 6000 bytes
	tel.observe(gopevents.Delta{Text: long})
	tel.observe(gopevents.Done{})
	out := pub.payload(t, events.KindSessionTurnOutput, 0)
	text, _ := out["text"].(string)
	if len(text) > turnEventTextCap || !utf8.ValidString(text) || out["truncated"] != true || out["text_bytes"] != float64(len(long)) {
		t.Fatalf("capped reply: %d bytes, valid=%v, truncated=%v, text_bytes=%v", len(text), utf8.ValidString(text), out["truncated"], out["text_bytes"])
	}
}

// A provider error ends the turn as session.turn_failed, with the usage
// already reported. A provider that reports one failure twice (Codex sends an
// error line and then a generic turn.failed) is still one failed turn, and a
// Done after it is not a second end. The next turn starts clean.
func TestTurnTelemetry_ProviderError(t *testing.T) {
	tel, pub := newTestTelemetry()
	tel.observe(gopevents.Delta{Text: "partial"})
	tel.observe(gopevents.Usage{InputTokens: 9})
	tel.observe(gopevents.Error{Message: "rate limited"})
	tel.observe(gopevents.Error{Message: "codex error"})
	tel.observe(gopevents.Done{})
	if got := strings.Join(pub.kinds(), ","); got != "session.turn_failed,provider.turn_usage" {
		t.Fatalf("published %s; want one turn_failed and the usage", got)
	}
	f := pub.payload(t, events.KindSessionTurnFailed, 0)
	if f["error"] != "rate limited" || f["truncated"] != false {
		t.Fatalf("turn_failed = %v; want the first, specific message", f)
	}
	if _, ok := f["exit_code"]; ok {
		t.Fatalf("a provider error carries no exit code: %v", f)
	}

	// The next turn is its own: it publishes, and fails on its own.
	tel.observe(gopevents.Delta{Text: "ok"})
	tel.observe(gopevents.Done{})
	tel.observe(gopevents.Error{Message: "second failure"})
	if got := strings.Join(pub.kinds(), ","); got != "session.turn_failed,provider.turn_usage,session.turn_output,session.turn_failed" {
		t.Fatalf("published %s", got)
	}
	if f := pub.payload(t, events.KindSessionTurnFailed, 1); f["error"] != "second failure" {
		t.Fatalf("second turn_failed = %v", f)
	}
}

// A subprocess turn is bracketed by its process: the provider's error is held
// until the process exits, so a failure is one session.turn_failed with the
// provider's message and the exit code; a failure with only an exit status
// carries the error, which has the bounded stderr tail; a clean exit
// without an end-of-turn event still publishes the output; a refusal (not a
// turn) publishes nothing.
func TestTurnTelemetry_SubprocessTurns(t *testing.T) {
	tel, pub := newTestTelemetry()
	tel.bracketed = true

	tel.beginTurn()
	exitErr := fmt.Errorf("agentsessions: %w\nstderr (last 30 bytes):\nError: 401 Unauthorized", &runner.ExitError{Code: 1})
	tel.endSubprocessTurn(exitErr)
	f := pub.payload(t, events.KindSessionTurnFailed, 0)
	if f["exit_code"] != 1.0 || !strings.Contains(f["error"].(string), "401 Unauthorized") {
		t.Fatalf("exit-only failure = %v", f)
	}

	// Codex: an error line, then a generic one, then a non-zero exit. One
	// event, with the specific message and the exit code.
	tel.beginTurn()
	tel.observe(gopevents.Usage{InputTokens: 5})
	tel.observe(gopevents.Error{Message: "The 'x' model is not supported"})
	tel.observe(gopevents.Error{Message: "codex error"})
	if n := strings.Count(strings.Join(pub.kinds(), ","), "session.turn_failed"); n != 1 {
		t.Fatalf("published %d failures before the process exited: %v", n-1, pub.kinds())
	}
	tel.endSubprocessTurn(fmt.Errorf("agentsessions: %w", &runner.ExitError{Code: 1}))
	f = pub.payload(t, events.KindSessionTurnFailed, 1)
	if f["error"] != "The 'x' model is not supported" || f["exit_code"] != 1.0 {
		t.Fatalf("provider-error failure = %v; want its message and exit code 1", f)
	}
	if u := pub.payload(t, events.KindProviderTurnUsage, 0); u["input_tokens"] != 5.0 {
		t.Fatalf("a failed turn's usage = %v", u)
	}

	// An error event but a zero exit is still a failed turn, with no code.
	tel.beginTurn()
	tel.observe(gopevents.Error{Message: "soft failure"})
	tel.endSubprocessTurn(nil)
	if f := pub.payload(t, events.KindSessionTurnFailed, 2); f["error"] != "soft failure" {
		t.Fatalf("zero-exit failure = %v", f)
	} else if _, ok := f["exit_code"]; ok {
		t.Fatalf("a zero exit carries no code: %v", f)
	}

	tel.beginTurn()
	tel.observe(gopevents.Delta{Text: "no done event"})
	tel.endSubprocessTurn(nil)
	if out := pub.payload(t, events.KindSessionTurnOutput, 0); out["text"] != "no done event" {
		t.Fatalf("clean exit without Done: %v", out)
	}

	n := len(pub.kinds())
	tel.beginTurn()
	tel.endSubprocessTurn(agentsessions.ErrTurnInFlight)
	tel.beginTurn()
	tel.endSubprocessTurn(nil)
	if len(pub.kinds()) != n {
		t.Fatalf("a refused or empty turn published %v", pub.kinds()[n:])
	}
}

func TestLaunchModel(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"--model", "gpt-5"}, "gpt-5"},
		{[]string{"-m", "o3"}, "o3"},
		{[]string{"--format", "json", "--model=anthropic/x"}, "anthropic/x"},
		{[]string{"--model"}, ""},
	} {
		if got := launchModel(tc.args); got != tc.want {
			t.Errorf("launchModel(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// End to end through LaunchSession and agentkit's adapter runtime: a codex
// exec turn publishes its reply and usage on the bus, and a failed turn
// publishes session.turn_failed with the exit code and stderr tail. The
// session.log lines are unchanged.
func TestTurnTelemetry_SubprocessSessionOnTheBus(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte(fakeCodex), 0o755); err != nil { //nolint:gosec // test stand-in must be executable
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const sessID = "sess-telemetry"
	plan := &launch.Plan{
		LaunchID:       "codex-launch",
		ProjectID:      "proj",
		LogicalAgentID: "agent",
		ProviderID:     "codex-cli",
		ProviderBrand:  "codex",
		RuntimeKind:    config.RuntimeKindSubprocess,
		RepoRoot:       t.TempDir(),
		WriteHome:      t.TempDir(),
		WorkspaceMode:  "shared",
		Command:        fake,
		Args:           []string{"--model", "gpt-5-codex"},
	}
	ws, err := workspace.Create(plan.WriteHome, sessID, plan)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	row := store.SessionRow{
		ID: sessID, LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID,
		ProviderID: plan.ProviderID, ProviderKind: "cli", Workspace: ws.Root, State: "created",
	}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatalf("create session: %v", err)
	}
	factory, err := runtimeFactoryForProvider(config.Provider{ID: "codex-cli", Adapter: "codex", RuntimeKind: config.RuntimeKindSubprocess})
	if err != nil {
		t.Fatalf("runtime factory: %v", err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	sub, cancel, err := bus.Subscribe(context.Background(), events.Filter{SessionID: sessID, Kinds: []string{
		events.KindProviderTurnUsage, events.KindSessionTurnOutput, events.KindSessionTurnFailed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
		Store:       db,
		Bus:         bus,
		Manager:     agentsessions.NewManager(stateSinkAdapter{db: db}),
		factories:   map[string]RuntimeFactory{"codex-cli": factory},
	}
	if _, err := svc.LaunchSession(sessID); err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), sessID) })

	next := func() events.Event {
		t.Helper()
		select {
		case e := <-sub:
			return e
		case <-time.After(10 * time.Second):
			t.Fatal("no turn event on the bus")
			return events.Event{}
		}
	}

	if err := svc.SendTurn(context.Background(), sessID, "hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	out, usage := next(), next()
	if out.Kind != events.KindSessionTurnOutput || !strings.Contains(out.PayloadJSON, `"text":"FAKE REPLY"`) {
		t.Fatalf("first event = %s %s; want session.turn_output with the reply", out.Kind, out.PayloadJSON)
	}
	if usage.Kind != events.KindProviderTurnUsage || !strings.Contains(usage.PayloadJSON, `"provider":"codex-cli"`) ||
		!strings.Contains(usage.PayloadJSON, `"model":"gpt-5-codex"`) || !strings.Contains(usage.PayloadJSON, `"input_tokens":1`) {
		t.Fatalf("second event = %s %s; want provider.turn_usage", usage.Kind, usage.PayloadJSON)
	}

	if err := svc.SendTurn(context.Background(), sessID, "please FAIL"); err == nil {
		t.Fatal("failing turn returned no error")
	}
	failed := next()
	if failed.Kind != events.KindSessionTurnFailed || !strings.Contains(failed.PayloadJSON, `"exit_code":1`) ||
		!strings.Contains(failed.PayloadJSON, "401 Unauthorized") || strings.Contains(failed.PayloadJSON, "PATH=") {
		t.Fatalf("failed turn event = %s %s; want session.turn_failed with exit code and stderr tail, no env", failed.Kind, failed.PayloadJSON)
	}

	logData := waitForLog(t, ws.LogPath, "401 Unauthorized")
	for _, want := range []string{"FAKE REPLY", "[turn_done]", "fake-codex: note on stderr"} {
		if !strings.Contains(logData, want) {
			t.Errorf("session.log missing %q:\n%s", want, logData)
		}
	}
}
