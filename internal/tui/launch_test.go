package tui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/tui/client"
)

func TestLaunchResultMsgPushesSuccessToast(t *testing.T) {
	m := NewMainScreen(nil)
	next, cmd := m.Update(launchResultMsg{
		req: client.CreateAndLaunchRequest{LaunchID: "demo-launch"},
		res: client.CreateAndLaunchResponse{SessionID: "abcdef01-2345-6789-abcd-ef0123456789", Workspace: "/tmp/ws"},
	})
	m = next.(MainScreen)
	if cmd == nil {
		t.Fatal("expected tea.Tick cmd for toast expiration")
	}
	toasts := m.toasts.items()
	if len(toasts) != 1 {
		t.Fatalf("expected 1 toast, got %d", len(toasts))
	}
	if toasts[0].Kind != ToastInfo {
		t.Fatalf("expected info toast, got %v", toasts[0].Kind)
	}
	if !strings.Contains(toasts[0].Message, "demo-launch") {
		t.Fatalf("expected launch id in toast, got %q", toasts[0].Message)
	}
	if !strings.Contains(toasts[0].Message, "abcdef01") {
		t.Fatalf("expected shortened session id in toast, got %q", toasts[0].Message)
	}
}

func TestLaunchResultMsgPushesErrorToast(t *testing.T) {
	m := NewMainScreen(nil)
	next, _ := m.Update(launchResultMsg{err: errors.New("daemon refused")})
	m = next.(MainScreen)
	toasts := m.toasts.items()
	if len(toasts) != 1 {
		t.Fatalf("expected 1 toast, got %d", len(toasts))
	}
	if toasts[0].Kind != ToastError {
		t.Fatalf("expected error toast, got %v", toasts[0].Kind)
	}
	if !strings.Contains(toasts[0].Message, "daemon refused") {
		t.Fatalf("expected error text in toast, got %q", toasts[0].Message)
	}
}

func TestToastExpiredRemovesToast(t *testing.T) {
	m := NewMainScreen(nil)
	next, _ := m.Update(launchResultMsg{
		res: client.CreateAndLaunchResponse{SessionID: "s1"},
	})
	m = next.(MainScreen)
	toasts := m.toasts.items()
	if len(toasts) != 1 {
		t.Fatalf("expected 1 toast, got %d", len(toasts))
	}
	id := toasts[0].ID

	next, _ = m.Update(toastExpiredMsg{ID: id})
	m = next.(MainScreen)
	if len(m.toasts.items()) != 0 {
		t.Fatalf("expected toast removed, got %d remaining", len(m.toasts.items()))
	}
}

func TestToastExpiredWrongIDIsNoOp(t *testing.T) {
	m := NewMainScreen(nil)
	next, _ := m.Update(launchResultMsg{res: client.CreateAndLaunchResponse{SessionID: "s1"}})
	m = next.(MainScreen)
	next, _ = m.Update(toastExpiredMsg{ID: 9999})
	m = next.(MainScreen)
	if len(m.toasts.items()) != 1 {
		t.Fatalf("expected toast unchanged, got %d", len(m.toasts.items()))
	}
}

func TestEnterOnNonLaunchRowOpensDetail(t *testing.T) {
	// Enter on a non-launch row does the same thing as Right-arrow:
	// opens the detail screen. The rule is "Enter always does
	// something" so the user is never left wondering whether Enter
	// is bound.
	m := newSeededModel(t)
	sel := m.SelectedRow()
	if _, ok := sel.(LaunchRow); ok {
		t.Skip("unexpected: top row is LaunchRow; test expects non-launch")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a push cmd from Enter on non-launch row")
	}
}

func TestEnterOnLaunchRowIssuesCreateAndLaunch(t *testing.T) {
	var createHit, launchHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			createHit = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sess-xyz","workspace":"/tmp/ws","log":"/tmp/ws/logs/session.log"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sessions/sess-xyz/launch":
			launchHit = true
			_, _ = w.Write([]byte(`{"id":"sess-xyz","workspace":"/tmp/ws","log":"/tmp/ws/logs/session.log"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	m := NewMainScreen(c)
	// Size the layout so the viewport has a valid dimension.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = next.(MainScreen)

	// Seed only a single LaunchRow so SelectedRow lands on it.
	next, _ = m.Update(catalogLoadedMsg{
		typ: RowTypeLaunches,
		rows: rowsFromLaunches([]config.Launch{
			{ID: "demo-launch", Project: "acme", Agent: "writer", Provider: "stub"},
		}),
	})
	m = next.(MainScreen)
	// Drain the remaining four load slots so loadRemaining hits zero.
	for _, typ := range []RowType{RowTypeProjects, RowTypeAgents, RowTypeProviders, RowTypeSessions} {
		next, _ = m.Update(catalogLoadedMsg{typ: typ})
		m = next.(MainScreen)
	}

	sel := m.SelectedRow()
	if _, ok := sel.(LaunchRow); !ok {
		t.Fatalf("expected LaunchRow selected, got %T", sel)
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a tea.Cmd from Enter on LaunchRow")
	}
	// Execute the cmd and inspect the returned message.
	msg := cmd()
	result, ok := msg.(launchResultMsg)
	if !ok {
		t.Fatalf("expected launchResultMsg, got %T", msg)
	}
	if result.err != nil {
		t.Fatalf("unexpected launch error: %v", result.err)
	}
	if result.res.SessionID != "sess-xyz" {
		t.Fatalf("expected sess-xyz, got %q", result.res.SessionID)
	}
	if !createHit || !launchHit {
		t.Fatalf("expected both create+launch endpoints hit; got create=%v launch=%v",
			createHit, launchHit)
	}
	_ = api.SessionDTO{} // silence unused import once above refactored
}
