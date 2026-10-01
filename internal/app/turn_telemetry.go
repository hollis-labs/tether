package app

import (
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runner/runner"

	"github.com/hollis-labs/tether/internal/events"
)

// turnEventTextCap bounds the text a turn event carries: the reply in
// session.turn_output and the error in session.turn_failed. The events table
// has opt-in retention but no size cap, so a turn event never carries a whole
// long reply; session.log keeps the full text.
const turnEventTextCap = 4096

// turnTelemetry turns one session's typed provider events into turn-level
// bus events (CW-20260930-0223, CW-20261001-0058):
//
//   - provider.turn_usage: the turn's summed usage. agentkit reports usage
//     per step (OpenCode reports one per step_finish) and deliberately keeps
//     no per-turn total of its own (D-71), so Tether sums input, output,
//     cache and cost here. A context-size total is never summed.
//   - session.turn_output: the turn's reply text, capped.
//   - session.turn_failed: a turn that failed, from the provider's own error
//     event or, for a subprocess runtime, from the turn process's non-zero
//     exit.
//
// It is fed by StartOptions.TypedEventCallback, which every agentkit runtime
// calls from its reader goroutine, and by the subprocess turn path; a mutex
// keeps the two in step. All three kinds are additive: no existing kind
// changes, and session.log is untouched.
type turnTelemetry struct {
	bus            events.Publisher
	sessionID      string
	logicalAgentID string
	provider       string
	model          string

	// bracketed is set for a runtime that runs one process per turn and
	// brackets each with beginTurn and endSubprocessTurn. A provider error
	// there is held until the process exits, so the failure is one event with
	// both the provider's message and the exit status.
	bracketed bool

	mu sync.Mutex
	// active is set by any event of the turn in progress and cleared when
	// the turn's events are published.
	active   bool
	usage    *turnUsage
	segments []*turnTextSegment
	// failedSeen is set by the first provider error of a turn, and cleared by
	// the first event of the next one: a provider that reports one failure
	// twice (Codex sends an error line and then a generic turn.failed) is
	// still one failed turn. errMsg is that first, most specific message.
	failedSeen bool
	errMsg     string
}

type turnUsage struct {
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CostUSD             float64 `json:"cost_usd,omitempty"`
	StopReason          string  `json:"stop_reason,omitempty"`
}

// turnTextSegment is one block of reply text. A provider that names its
// blocks (an id) may resend a block as it grows, so the latest text for an
// id wins; unnamed deltas are streaming chunks and append.
type turnTextSegment struct {
	id    string
	final bool
	text  strings.Builder
}

func newTurnTelemetry(bus events.Publisher, sessionID, logicalAgentID, provider, model string) *turnTelemetry {
	return &turnTelemetry{bus: bus, sessionID: sessionID, logicalAgentID: logicalAgentID, provider: provider, model: model}
}

// observe takes one typed provider event.
func (t *turnTelemetry) observe(ev gopevents.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch e := ev.(type) {
	case gopevents.Delta:
		t.startTurnEvent()
		t.addText(e)
	case gopevents.Usage:
		t.startTurnEvent()
		t.addUsage(e)
	case gopevents.ToolUse, gopevents.ToolResult, gopevents.Thinking:
		t.startTurnEvent()
	case gopevents.Done:
		if !t.active || t.failedSeen {
			return
		}
		if e.StopReason != "" && t.usage != nil && t.usage.StopReason == "" {
			t.usage.StopReason = e.StopReason
		}
		t.publishOutput(e.StopReason)
		t.publishUsage()
		t.reset()
	case gopevents.Error:
		if t.failedSeen {
			return
		}
		msg := e.Message
		if msg == "" && e.Err != nil {
			msg = e.Err.Error()
		}
		t.failedSeen, t.errMsg = true, msg
		if t.bracketed {
			return
		}
		t.publishFailed(msg, nil)
		t.publishUsage()
		t.reset()
	}
}

// startTurnEvent marks an event of the turn in progress. The first one after
// a failed turn starts a new turn.
func (t *turnTelemetry) startTurnEvent() {
	t.failedSeen, t.errMsg = false, ""
	t.active = true
}

// beginTurn marks the start of a turn Tether sent itself (a subprocess
// turn). Anything left from a turn that never reached its end is dropped.
func (t *turnTelemetry) beginTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reset()
}

// endSubprocessTurn closes a subprocess turn once its process has exited.
//
//   - A failed turn is one session.turn_failed: the provider's own error
//     message when it sent one, otherwise the error, which carries the turn's
//     bounded stderr tail (see subprocessLog), with the exit code when the
//     process exited non-zero.
//   - A clean exit with no provider error publishes whatever the turn
//     produced when the provider never sent its own end-of-turn event.
//   - Any other error is a refusal, not a turn, and publishes nothing.
func (t *turnTelemetry) endSubprocessTurn(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var exit *runner.ExitError
	switch {
	case errors.As(err, &exit):
		msg := t.errMsg
		if msg == "" {
			msg = err.Error()
		}
		code := exit.Code
		t.publishFailed(msg, &code)
		t.publishUsage()
	case err != nil:
		t.reset()
		return
	case t.failedSeen:
		t.publishFailed(t.errMsg, nil)
		t.publishUsage()
	case t.active:
		t.publishOutput("")
		t.publishUsage()
	}
	t.reset()
}

func (t *turnTelemetry) reset() {
	t.active = false
	t.usage = nil
	t.segments = nil
	if t.bracketed {
		t.failedSeen, t.errMsg = false, ""
	}
}

func (t *turnTelemetry) addText(d gopevents.Delta) {
	if d.Text == "" {
		return
	}
	final := d.Phase == "final"
	if d.BlockID != "" {
		for i := range t.segments {
			if t.segments[i].id == d.BlockID {
				t.segments[i].text.Reset()
				t.segments[i].text.WriteString(d.Text)
				t.segments[i].final = t.segments[i].final || final
				return
			}
		}
		seg := &turnTextSegment{id: d.BlockID, final: final}
		seg.text.WriteString(d.Text)
		t.segments = append(t.segments, seg)
		return
	}
	if n := len(t.segments); n > 0 && t.segments[n-1].id == "" && t.segments[n-1].final == final {
		t.segments[n-1].text.WriteString(d.Text)
		return
	}
	seg := &turnTextSegment{final: final}
	seg.text.WriteString(d.Text)
	t.segments = append(t.segments, seg)
}

func (t *turnTelemetry) addUsage(u gopevents.Usage) {
	if t.usage == nil {
		t.usage = &turnUsage{}
	}
	t.usage.InputTokens += u.InputTokens
	t.usage.OutputTokens += u.OutputTokens
	t.usage.CacheCreationTokens += u.CacheCreationTokens
	t.usage.CacheReadTokens += u.CacheReadTokens
	// CostUSD is a per-event delta (go-llm-types), so the sum is the turn's
	// cost.
	t.usage.CostUSD += u.CostUSD
	if u.StopReason != "" {
		t.usage.StopReason = u.StopReason
	}
}

// replyText is the turn's reply: the blocks a provider marked final when it
// marked any (Codex sends its narration and then the final message), and
// otherwise every block, in order.
func (t *turnTelemetry) replyText() string {
	anyFinal := false
	for i := range t.segments {
		if t.segments[i].final {
			anyFinal = true
			break
		}
	}
	parts := make([]string, 0, len(t.segments))
	for i := range t.segments {
		if anyFinal && !t.segments[i].final {
			continue
		}
		parts = append(parts, t.segments[i].text.String())
	}
	return strings.Join(parts, "\n")
}

func (t *turnTelemetry) publishOutput(stopReason string) {
	text := t.replyText()
	capped, truncated := capText(text, turnEventTextCap)
	publishSessionEvent(t.bus, t.sessionID, t.logicalAgentID, events.KindSessionTurnOutput, struct {
		SessionID  string `json:"session_id"`
		Text       string `json:"text"`
		TextBytes  int    `json:"text_bytes"`
		Truncated  bool   `json:"truncated"`
		StopReason string `json:"stop_reason,omitempty"`
	}{t.sessionID, capped, len(text), truncated, stopReason})
}

func (t *turnTelemetry) publishFailed(msg string, exitCode *int) {
	capped, truncated := capText(msg, turnEventTextCap)
	publishSessionEvent(t.bus, t.sessionID, t.logicalAgentID, events.KindSessionTurnFailed, struct {
		SessionID string `json:"session_id"`
		Error     string `json:"error"`
		Truncated bool   `json:"truncated"`
		ExitCode  *int   `json:"exit_code,omitempty"`
	}{t.sessionID, capped, truncated, exitCode})
}

func (t *turnTelemetry) publishUsage() {
	if t.usage == nil {
		return
	}
	publishSessionEvent(t.bus, t.sessionID, t.logicalAgentID, events.KindProviderTurnUsage, struct {
		SessionID string `json:"session_id"`
		Provider  string `json:"provider"`
		Model     string `json:"model,omitempty"`
		turnUsage
	}{t.sessionID, t.provider, t.model, *t.usage})
}

// capText returns s cut to at most n bytes on a rune boundary, and whether
// it was cut.
func capText(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// launchModel is the model a launch selected on its command line with
// --model or -m, or "" when it left the provider's default. The typed usage
// events carry no model of their own.
func launchModel(args []string) string {
	for i, a := range args {
		switch {
		case a == "--model" || a == "-m":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--model="):
			return strings.TrimPrefix(a, "--model=")
		}
	}
	return ""
}
