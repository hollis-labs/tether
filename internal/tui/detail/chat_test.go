package detail

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/screen"
	"github.com/chrispian/agent-mux/pkg/claudestream"
)

// newTestChat builds a ChatScreen primed for reducer tests — no daemon
// client, sized, with ctx/ch wired so Update paths don't panic.
func newTestChat(t *testing.T) *ChatScreen {
	t.Helper()
	s := NewChatScreen(
		api.SessionDTO{
			ID:         "abc12345-dead-beef-0000-000000000000",
			Workspace:  "/tmp/ws",
			State:      "running",
			ProviderID: "claude-stream",
		},
		nil,
	)
	s.width, s.height = 120, 30
	s.resize()
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel = ctx, cancel
	s.ch = make(chan chatStreamMsg, 8)
	return s
}

func typeRunes(t *testing.T, s *ChatScreen, text string) *ChatScreen {
	t.Helper()
	for _, r := range text {
		next, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		s = next.(*ChatScreen)
	}
	return s
}

func TestChatEnterSendsTurnAndSetsAwaiting(t *testing.T) {
	s := newTestChat(t)
	s = typeRunes(t, s, "hello claude")
	if s.input.Value() != "hello claude" {
		t.Fatalf("expected input populated, got %q", s.input.Value())
	}

	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)

	if len(s.history) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(s.history))
	}
	if s.history[0].user != "hello claude" {
		t.Fatalf("expected user=%q, got %q", "hello claude", s.history[0].user)
	}
	if !s.awaiting {
		t.Fatal("expected awaiting=true after Enter")
	}
	if s.input.Value() != "" {
		t.Fatalf("expected input cleared, got %q", s.input.Value())
	}
}

func TestChatEnterNoOpWhileAwaiting(t *testing.T) {
	s := newTestChat(t)
	s = typeRunes(t, s, "first")
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)

	// Now try to send a second turn while awaiting is still true.
	s = typeRunes(t, s, "second")
	before := s.input.Value()
	next, _ = s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)

	if len(s.history) != 1 {
		t.Fatalf("expected still 1 turn while awaiting, got %d", len(s.history))
	}
	if s.input.Value() != before {
		t.Fatalf("expected input preserved while awaiting, got %q", s.input.Value())
	}
	if !strings.Contains(s.status, "waiting") {
		t.Fatalf("expected status flash, got %q", s.status)
	}
}

func TestChatEnterEmptyInputIsNoOp(t *testing.T) {
	s := newTestChat(t)
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)
	if len(s.history) != 0 {
		t.Fatalf("expected no turn on empty input, got %d", len(s.history))
	}
	if s.awaiting {
		t.Fatal("expected awaiting=false after empty Enter")
	}
}

func TestChatDeltaEventsAppendToCurrentTurn(t *testing.T) {
	s := newTestChat(t)
	s = typeRunes(t, s, "hi")
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)

	next, _ = s.Update(chatStreamMsg{ev: claudestream.Event{Kind: claudestream.KindDelta, Text: "Hello"}})
	s = next.(*ChatScreen)
	next, _ = s.Update(chatStreamMsg{ev: claudestream.Event{Kind: claudestream.KindDelta, Text: ", world."}})
	s = next.(*ChatScreen)

	turn := s.history[len(s.history)-1]
	if len(turn.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(turn.events))
	}
	body := s.renderHistory()
	if !strings.Contains(body, "Hello, world.") {
		t.Fatalf("expected concatenated deltas in body, got:\n%s", body)
	}
}

func TestChatKindDoneClearsAwaitingAndMarksTurnDone(t *testing.T) {
	s := newTestChat(t)
	s = typeRunes(t, s, "hi")
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)

	next, _ = s.Update(chatStreamMsg{ev: claudestream.Event{Kind: claudestream.KindDone}})
	s = next.(*ChatScreen)

	if s.awaiting {
		t.Fatal("expected awaiting=false after KindDone")
	}
	if !s.history[len(s.history)-1].done {
		t.Fatal("expected last turn done=true")
	}
}

func TestChatKindUsageAccumulatesAcrossTurns(t *testing.T) {
	s := newTestChat(t)
	s = typeRunes(t, s, "turn1")
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)

	next, _ = s.Update(chatStreamMsg{ev: claudestream.Event{
		Kind:  claudestream.KindUsage,
		Usage: &claudestream.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3},
	}})
	s = next.(*ChatScreen)
	next, _ = s.Update(chatStreamMsg{ev: claudestream.Event{Kind: claudestream.KindDone}})
	s = next.(*ChatScreen)
	next, _ = s.Update(chatStreamMsg{ev: claudestream.Event{
		Kind:  claudestream.KindUsage,
		Usage: &claudestream.Usage{InputTokens: 5, OutputTokens: 7, CacheReadTokens: 1},
	}})
	s = next.(*ChatScreen)

	if got := s.usageCumul.InputTokens; got != 15 {
		t.Fatalf("expected input tokens 15, got %d", got)
	}
	if got := s.usageCumul.OutputTokens; got != 27 {
		t.Fatalf("expected output tokens 27, got %d", got)
	}
	if got := s.usageCumul.CacheReadTokens; got != 4 {
		t.Fatalf("expected cache-read tokens 4, got %d", got)
	}
	if !strings.Contains(s.headerLine(), "in:15 out:27 cache:4") {
		t.Fatalf("expected cumulative tokens in header, got %q", s.headerLine())
	}
}

func TestChatToolUseTabFocusAndEnterTogglesExpand(t *testing.T) {
	s := newTestChat(t)
	s = typeRunes(t, s, "run")
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)

	toolID := "toolu_01"
	next, _ = s.Update(chatStreamMsg{ev: claudestream.Event{
		Kind:    claudestream.KindToolUse,
		ToolUse: &claudestream.ToolUseBlock{ID: toolID, Name: "Read", Input: map[string]any{"path": "/x"}},
	}})
	s = next.(*ChatScreen)

	// Tab cycles focus from input (-1) to the first tool_use (0).
	next, _ = s.Update(tea.KeyMsg{Type: tea.KeyTab})
	s = next.(*ChatScreen)
	if s.toolFocus != 0 {
		t.Fatalf("expected toolFocus=0 after Tab, got %d", s.toolFocus)
	}

	// Enter while focused on a tool_use toggles expanded.
	next, _ = s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)
	if !s.expanded[toolID] {
		t.Fatal("expected expanded[toolID]=true after Enter")
	}
	body := s.renderHistory()
	if !strings.Contains(body, "\"path\"") {
		t.Fatalf("expected expanded body to show tool input JSON, got:\n%s", body)
	}

	// Enter again collapses.
	next, _ = s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)
	if s.expanded[toolID] {
		t.Fatal("expected collapse after second Enter")
	}
}

func TestChatEnterOnFocusedToolDoesNotSendTurn(t *testing.T) {
	s := newTestChat(t)
	// Pre-seed a completed turn with a tool_use so Tab has a target.
	s.history = append(s.history, chatTurn{
		user: "run",
		events: []claudestream.Event{{
			Kind:    claudestream.KindToolUse,
			ToolUse: &claudestream.ToolUseBlock{ID: "t1", Name: "Bash"},
		}, {Kind: claudestream.KindDone}},
		done: true,
	})

	// Focus the tool, type (which should be ignored) then Enter.
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyTab})
	s = next.(*ChatScreen)
	s.input.SetValue("should-not-send")

	before := len(s.history)
	next, _ = s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)
	if len(s.history) != before {
		t.Fatalf("expected no new turn when focused on tool_use, got %d", len(s.history))
	}
	if s.awaiting {
		t.Fatal("expected awaiting=false when Enter only toggled a tool block")
	}
}

func TestChatEscDetachesAndPops(t *testing.T) {
	s := newTestChat(t)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected Pop cmd on Esc")
	}
	if _, ok := cmd().(screen.PopScreenMsg); !ok {
		t.Fatalf("expected PopScreenMsg, got %T", cmd())
	}
	if s.ctx.Err() == nil {
		t.Fatal("expected ctx canceled after Esc")
	}
}

func TestChatStreamDoneMarksFinished(t *testing.T) {
	s := newTestChat(t)
	next, _ := s.Update(chatStreamMsg{done: true})
	s = next.(*ChatScreen)
	if !s.finished {
		t.Fatal("expected finished=true after done msg")
	}
}

func TestChatKindSessionIDCaptured(t *testing.T) {
	s := newTestChat(t)
	const sid = "bf32b940-fc2a-4d55-a9cb-66a758b75c98"
	next, _ := s.Update(chatStreamMsg{ev: claudestream.Event{Kind: claudestream.KindSessionID, SessionID: sid}})
	s = next.(*ChatScreen)
	if s.sessionID != sid {
		t.Fatalf("expected sessionID captured, got %q", s.sessionID)
	}
	if !strings.Contains(s.renderHistory(), "session "+shortID(sid)) {
		t.Fatalf("expected system/init marker rendered, got:\n%s", s.renderHistory())
	}
}

func TestChatKindErrorRendersInBody(t *testing.T) {
	s := newTestChat(t)
	next, _ := s.Update(chatStreamMsg{ev: claudestream.Event{Kind: claudestream.KindError, ErrorMsg: "rate limited"}})
	s = next.(*ChatScreen)
	if !strings.Contains(s.renderHistory(), "rate limited") {
		t.Fatalf("expected error text in body, got:\n%s", s.renderHistory())
	}
}

func TestChatSendErrMsgClearsAwaiting(t *testing.T) {
	s := newTestChat(t)
	s = typeRunes(t, s, "hi")
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*ChatScreen)
	if !s.awaiting {
		t.Fatal("precondition: expected awaiting=true")
	}
	next, _ = s.Update(chatSendErrMsg{err: errDummy})
	s = next.(*ChatScreen)
	if s.awaiting {
		t.Fatal("expected awaiting cleared after send error")
	}
	if !strings.Contains(s.status, "send failed") {
		t.Fatalf("expected status to report send failure, got %q", s.status)
	}
}
