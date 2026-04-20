package detail

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

var errDummy = errors.New("dummy")

// newTestAttach returns an AttachScreen suitable for reducer tests —
// no daemon client, pre-sized, with the context/channel pair
// initialized so handleKey and attachMsg paths work the same way they
// would in a live session.
func newTestAttach(t *testing.T) *AttachScreen {
	t.Helper()
	s := NewAttachScreen(api.SessionDTO{ID: "abc12345-dead-beef-0000-000000000000", State: "running", Workspace: "/tmp/ws"}, nil)
	s.width, s.height = 120, 30
	s.resize()
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel = ctx, cancel
	s.ch = make(chan attachMsg, 4)
	return s
}

func TestAttachAppendBytesAccumulates(t *testing.T) {
	s := newTestAttach(t)
	next, _ := s.Update(attachMsg{bytes: []byte("hello ")})
	s = next.(*AttachScreen)
	next, _ = s.Update(attachMsg{bytes: []byte("world\n")})
	s = next.(*AttachScreen)
	if got := string(s.buf); got != "hello world\n" {
		t.Fatalf("expected concatenated buffer, got %q", got)
	}
}

func TestAttachBufferTrimAtCap(t *testing.T) {
	s := newTestAttach(t)
	// Push slightly over cap.
	big := strings.Repeat("x", attachBufCap+128)
	next, _ := s.Update(attachMsg{bytes: []byte(big)})
	s = next.(*AttachScreen)
	if len(s.buf) != attachBufCap {
		t.Fatalf("expected buffer capped at %d, got %d", attachBufCap, len(s.buf))
	}
}

func TestAttachEscPopsAndCancels(t *testing.T) {
	s := newTestAttach(t)
	next, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_ = next
	if cmd == nil {
		t.Fatal("expected Pop cmd on Esc")
	}
	if _, ok := cmd().(screen.PopScreenMsg); !ok {
		t.Fatalf("expected PopScreenMsg, got %T", cmd())
	}
	// Ctx cancelation is best-effort visible via Err().
	if s.ctx.Err() == nil {
		t.Fatal("expected ctx canceled after Esc detach")
	}
}

func TestAttachCtrlCDoesNotQuitInsteadForwardsSIGINT(t *testing.T) {
	s := newTestAttach(t)
	// Without a live client SendInput won't fire; the cmd returned
	// should be nil (sendBytesCmd returns nil when client == nil).
	// The important property is that Ctrl-C does NOT emit tea.Quit.
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("Ctrl-C while attached should NOT quit the TUI")
		}
	}
}

func TestAttachEnterCapturesAndClearsInput(t *testing.T) {
	s := newTestAttach(t)
	// Type "hi".
	next, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	s = next.(*AttachScreen)
	next, _ = s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	s = next.(*AttachScreen)
	if s.input.Value() != "hi" {
		t.Fatalf("expected input 'hi', got %q", s.input.Value())
	}
	// Enter should clear the input.
	next, _ = s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s = next.(*AttachScreen)
	if s.input.Value() != "" {
		t.Fatalf("expected input cleared after Enter, got %q", s.input.Value())
	}
}

func TestAttachDoneMsgSetsFinished(t *testing.T) {
	s := newTestAttach(t)
	next, _ := s.Update(attachMsg{done: true})
	s = next.(*AttachScreen)
	if !s.finished {
		t.Fatal("expected finished=true after done msg")
	}
}

func TestAttachWindowSizeEmitsResize(t *testing.T) {
	// Needs a non-nil client so resize cmd fires. Use a dead address
	// so the cmd does not actually post — we only assert that a cmd
	// was returned; execution would hit the dead address and produce
	// an error msg we don't care about here.
	c := client.New("tcp:127.0.0.1:1")
	s := NewAttachScreen(api.SessionDTO{ID: "abc"}, c)
	// Wire the goroutine plumbing so Update doesn't panic on close.
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx, s.cancel = ctx, cancel
	s.ch = make(chan attachMsg, 4)
	t.Cleanup(cancel)

	_, cmd := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if cmd == nil {
		t.Fatal("expected resize cmd from WindowSizeMsg, got nil")
	}
}

func TestAttachWindowSizeNilClientNoOp(t *testing.T) {
	s := newTestAttach(t) // client is nil
	_, cmd := s.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if cmd != nil {
		t.Fatalf("expected nil cmd for nil client, got %T", cmd())
	}
}

func TestAttachResizeFailureSetsStatus(t *testing.T) {
	s := newTestAttach(t)
	next, _ := s.Update(attachResizeResultMsg{err: errDummy})
	s = next.(*AttachScreen)
	if !strings.Contains(s.status, "resize failed") {
		t.Fatalf("expected resize failure status, got %q", s.status)
	}
}

func TestSessionScreenPushesAttachOnAKey(t *testing.T) {
	// SessionScreen with nil client should NOT push attach (client
	// needed for streaming).
	ss := NewSessionScreen(api.SessionDTO{ID: "s1"}, nil)
	// Size it via window msg so base.width/height are non-zero.
	next, _ := ss.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	ss = next.(SessionScreen)
	_, cmd := ss.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if cmd != nil {
		if _, ok := cmd().(screen.PushScreenMsg); ok {
			t.Fatal("expected no Push when client is nil")
		}
	}
}
